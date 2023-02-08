/*
Copyright 2021 k0s authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package supervisor

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/k0sproject/k0s/internal/pkg/dir"
	"github.com/k0sproject/k0s/pkg/constant"
)

// Supervisor is dead simple and stupid process supervisor, just tries to keep the process running in a while-true loop
type Supervisor struct {
	Name           string
	BinPath        string
	RunDir         string
	DataDir        string
	Args           []string
	PidFile        string
	UID            int
	GID            int
	TimeoutStop    time.Duration
	TimeoutRespawn time.Duration
	// For those components having env prefix convention such as ETCD_xxx, we should keep the prefix.
	KeepEnvPrefix bool

	cmd   *exec.Cmd
	quit  chan bool
	done  chan bool
	log   logrus.FieldLogger
	mutex sync.Mutex
}

// processWaitQuit waits for a process to exit or a shut down signal
// returns true if shutdown is requested
func (s *Supervisor) processWaitQuit() bool {
	waitresult := make(chan error)
	go func() {
		waitresult <- s.cmd.Wait()
	}()

	pidbuf := []byte(strconv.Itoa(s.cmd.Process.Pid) + "\n")
	err := os.WriteFile(s.PidFile, pidbuf, constant.PidFileMode)
	if err != nil {
		s.log.Warnf("Failed to write file %s: %v", s.PidFile, err)
	}
	defer os.Remove(s.PidFile)

	select {
	case <-s.quit:
		for {
			s.log.Infof("Shutting down pid %d", s.cmd.Process.Pid)
			err := s.cmd.Process.Signal(syscall.SIGTERM)
			if err != nil {
				s.log.Warnf("Failed to send SIGTERM to pid %d: %s", s.cmd.Process.Pid, err)
			}
			select {
			case <-time.After(s.TimeoutStop):
				continue
			case <-waitresult:
				return true
			}
		}
	case err := <-waitresult:
		if err != nil {
			s.log.Warn(err)
		} else {
			s.log.Warnf("Process exited with code: %d", s.cmd.ProcessState.ExitCode())
		}
	}
	return false
}

// Supervise Starts supervising the given process
func (s *Supervisor) Supervise() error {
	s.log = logrus.WithField("component", s.Name)
	s.PidFile = path.Join(s.RunDir, s.Name) + ".pid"
	if err := dir.Init(s.RunDir, constant.RunDirMode); err != nil {
		s.log.Warnf("failed to initialize dir: %v", err)
		return err
	}

	if s.TimeoutStop == 0 {
		s.TimeoutStop = 5 * time.Second
	}
	if s.TimeoutRespawn == 0 {
		s.TimeoutRespawn = 5 * time.Second
	}

	started := make(chan error)
	go func() {
		s.log.Info("Starting to supervise")
		for {
			s.mutex.Lock()
			s.cmd = exec.Command(s.BinPath, s.Args...)
			s.cmd.Dir = s.DataDir
			s.cmd.Env = getEnv(s.DataDir, s.Name, s.KeepEnvPrefix)

			// detach from the process group so children don't
			// get signals sent directly to parent.
			s.cmd.SysProcAttr = DetachAttr(s.UID, s.GID)

			const maxLogChunkLen = 16 * 1024
			s.cmd.Stdout = &logWriter{
				log: s.log.WithField("stream", "stdout"),
				buf: make([]byte, maxLogChunkLen),
			}
			s.cmd.Stderr = &logWriter{
				log: s.log.WithField("stream", "stderr"),
				buf: make([]byte, maxLogChunkLen),
			}

			err := s.cmd.Start()
			s.mutex.Unlock()
			if err != nil {
				s.log.Warnf("Failed to start: %s", err)
				if s.quit == nil {
					started <- err
					return
				}
			} else {
				if s.quit == nil {
					s.log.Info("Started successfully, go nuts")
					s.quit = make(chan bool)
					s.done = make(chan bool)
					defer func() {
						s.done <- true
					}()
					started <- nil
				} else {
					s.log.Info("Restarted")
				}
				if s.processWaitQuit() {
					return
				}
			}

			// TODO Maybe some backoff thingy would be nice
			s.log.Infof("respawning in %s", s.TimeoutRespawn.String())

			select {
			case <-s.quit:
				s.log.Debug("respawn cancelled")
				return
			case <-time.After(s.TimeoutRespawn):
				s.log.Debug("respawning")
			}
		}
	}()
	return <-started
}

// Stop stops the supervised
func (s *Supervisor) Stop() error {
	if s.quit != nil {
		if s.log != nil {
			s.log.Debug("Sending stop message")
		}
		s.quit <- true
		if s.log != nil {
			s.log.Debug("Waiting for stopping is done")
		}
		<-s.done
	}
	return nil
}

// Prepare the env for exec:
// - handle component specific env
// - inject k0s embedded bins into path
func getEnv(dataDir, component string, keepEnvPrefix bool) []string {
	env := os.Environ()
	componentPrefix := fmt.Sprintf("%s_", strings.ToUpper(component))

	// put the component specific env vars in the front.
	sort.Slice(env, func(i, j int) bool { return strings.HasPrefix(env[i], componentPrefix) })

	overrides := map[string]struct{}{}
	i := 0
	for _, e := range env {
		kv := strings.SplitN(e, "=", 2)
		k, v := kv[0], kv[1]
		// if there is already a correspondent component specific env, skip it.
		if _, ok := overrides[k]; ok {
			continue
		}
		if strings.HasPrefix(k, componentPrefix) {
			var shouldOverride bool
			k1 := strings.TrimPrefix(k, componentPrefix)
			switch k1 {
			// always override proxy env
			case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY":
				shouldOverride = true
			default:
				if !keepEnvPrefix {
					shouldOverride = true
				}
			}
			if shouldOverride {
				k = k1
				overrides[k] = struct{}{}
			}
		}
		env[i] = fmt.Sprintf("%s=%s", k, v)
		if k == "PATH" {
			env[i] = fmt.Sprintf("PATH=%s:%s", path.Join(dataDir, "bin"), v)
		}
		i++
	}

	return env[:i]
}

// GetProcess returns the last started process
func (s *Supervisor) GetProcess() *os.Process {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.cmd.Process
}
