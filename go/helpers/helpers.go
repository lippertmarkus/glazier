// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package helpers provides miscellaneous helper functionality to other diagnose_me packages.
package helpers

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/mgr"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows"
	"github.com/google/logger"
	"github.com/iamacarpet/go-win64api"
)

// ExecResult holds the output from a subprocess execution.
type ExecResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	ExitErr  error
}

// ExecError holds errors and output from failed subprocess executions.
type ExecError struct {
	errmsg     string
	procresult ExecResult
}

// ExecConfig provides flexible execution configuration.
type ExecConfig struct {
	// A verifier, if specified, will attempt to verify the subprocess output and return an error if problems are detected.
	Verifier *ExecVerifier

	// A timeout will kill the subprocess if it doesn't execute within the duration. Leave nil to disable timeout.
	Timeout *time.Duration

	// If RetryCount is non-zero, Exec will attempt to retry a failed execution (one which returns err). Executions will retry
	// every RetryInterval. Combine with a Verifier to retry until certain conditions are met.
	RetryCount    int
	RetryInterval *time.Duration

	SpAttr *syscall.SysProcAttr

	// WriteStdOut and WriteStdErr will receive a copy of the child process's output and/or error streams as they're written.
	// If nil, output is returned at the end of execution via the ExecResult.
	WriteStdOut io.Writer
	WriteStdErr io.Writer
}

var (
	// ErrExitCode indicates an exit-code related failure
	ErrExitCode = errors.New("produced invalid exit code")
	// ErrStdErr indicates an problem with an executables stderr content
	ErrStdErr = errors.New("problem detected in error output")
	// ErrStdOut indicates an problem with an executables stdout content
	ErrStdOut = errors.New("problem detected in output")
	// ErrTimeout indicates a timeout related failure
	ErrTimeout = errors.New("time limit reached, killed executable")

	// PsPath contains the full path to Windows Powershell.
	PsPath = os.ExpandEnv("${windir}\\System32\\WindowsPowerShell\\v1.0\\powershell.exe")

	// TestHelpers
	fnExec        = execute
	fnProcessList = winapi.ProcessList
	fnSleep       = time.Sleep
)

// Satisfies the Error interface and returns the simple error string associated with the execution.
func (e ExecError) Error() string {
	return e.errmsg
}

// Result returns any information that could be captured from the subprocess that was executed.
func (e ExecError) Result() ExecResult {
	return e.procresult
}

// Exec executes a subprocess and returns the results.
//
// If Exec is called without a configuration, a default configuration is used. The default
// configuration will use a simple exit code verifier and no timeout. Behaviors can be disabled
// by supplying a config but leaving individual members as nil.
func Exec(path string, args []string, conf *ExecConfig) (ExecResult, error) {
	var err error
	var res ExecResult

	// Default config if unspecified.
	if conf == nil {
		conf = &ExecConfig{
			Verifier: NewExecVerifier(),
		}
	}
	// Default retry if unspecified.
	if conf.RetryInterval == nil {
		defInt := 1 * time.Minute
		conf.RetryInterval = &defInt
	}

	for attempt := 0; attempt <= conf.RetryCount; attempt++ {
		if res, err = fnExec(path, args, conf); err == nil {
			break
		}
		logger.Warningf("%s did not complete successfully: %v", path, err)
		if attempt == conf.RetryCount {
			break
		}
		logger.Infof("retrying in %v", conf.RetryInterval)
		fnSleep(*conf.RetryInterval)
	}
	return res, err
}

// ExecWithAttr executes a subprocess with custom process attributes and returns the results.
//
// See also https://github.com/golang/go/issues/17149.
func ExecWithAttr(path string, timeout *time.Duration, spattr *syscall.SysProcAttr) (ExecResult, error) {
	conf := &ExecConfig{
		Timeout: timeout,
		SpAttr:  spattr,
	}
	return fnExec(path, []string{}, conf)
}

// ExecVerifier provides checks against executable results.
//
// SuccessCodes specifies which exit codes are considered successful.
// StdErrMatch, if present, attempts to match specific strings in stderr, and if found, treats them as a failure.
// StdOutMatch, if present, attempts to match specific strings in stdout, and if found, treats them as a failure.
type ExecVerifier struct {
	SuccessCodes []int
	StdOutMatch  *regexp.Regexp
	StdErrMatch  *regexp.Regexp
}

// NewExecVerifier applys the default values to an ExecVerifier.
func NewExecVerifier() *ExecVerifier {
	return &ExecVerifier{
		SuccessCodes: []int{0},
	}
}

// ExecWithVerify executes a subprocess and performs additional verification on the results.
//
// Exec can return failures in multiple ways: explicit errors, invalid exit codes, error messages in outputs, etc.
// Without additional verification, these have to be checked individually by the caller. ExecWithVerify provides
// a wrapper that will perform most of these checks and will populate err if *any* of them are present, saving the caller
// the extra effort.
func ExecWithVerify(path string, args []string, timeout *time.Duration, verifier *ExecVerifier) (ExecResult, error) {
	if verifier == nil {
		verifier = NewExecVerifier()
	}
	conf := &ExecConfig{
		Timeout:  timeout,
		Verifier: verifier,
	}
	return fnExec(path, args, conf)
}

func verify(path string, res ExecResult, verifier ExecVerifier) (ExecResult, error) {
	if res.ExitErr != nil {
		return res, res.ExitErr
	}
	codeOk := false
	for _, c := range verifier.SuccessCodes {
		if c == res.ExitCode {
			codeOk = true
			break
		}
	}
	if !codeOk {
		return res, fmt.Errorf("%q %w: %d", path, ErrExitCode, res.ExitCode)
	}

	if verifier.StdErrMatch != nil && verifier.StdErrMatch.Match(res.Stderr) {
		return res, fmt.Errorf("%w from %q", ErrStdErr, path)
	}
	if verifier.StdOutMatch != nil && verifier.StdOutMatch.Match(res.Stdout) {
		return res, fmt.Errorf("%w from %q", ErrStdOut, path)
	}
	return res, nil
}

func execute(path string, args []string, conf *ExecConfig) (ExecResult, error) {
	var cmd *exec.Cmd
	result := ExecResult{}
	if conf == nil {
		return result, errors.New("conf cannot be nil")
	}

	switch strings.ToLower(filepath.Ext(path)) {
	case ".ps1":
		// Escape spaces in PowerShell paths.
		args = append([]string{"-NoProfile", "-NoLogo", "-Command", strings.ReplaceAll(path, " ", "` ")}, args...)
		// Append $LASTEXITCODE so exitcode can be inferred.
		// ref: https://docs.microsoft.com/en-us/powershell/module/microsoft.powershell.core/about/about_powershell_exe
		args = append(args, ";", "exit", "$LASTEXITCODE")
		path = PsPath
	case ".exe", ".bat":
		// path and args unmodified
	default:
		return result, errors.New("extension not currently supported")
	}

	if conf.SpAttr != nil {
		cmd = exec.Command(path)
		cmd.SysProcAttr = conf.SpAttr
	} else {
		cmd = exec.Command(path, args...)
	}

	// create our own buffer to hold a copy of the output and err
	var errbuf, outbuf bytes.Buffer

	// add our buffers to any supplied by the user and pass to cmd
	if conf.WriteStdOut != nil {
		cmd.Stdout = io.MultiWriter(&outbuf, conf.WriteStdOut)
	} else {
		cmd.Stdout = &outbuf
	}
	if conf.WriteStdErr != nil {
		cmd.Stderr = io.MultiWriter(&errbuf, conf.WriteStdErr)
	} else {
		cmd.Stderr = &errbuf
	}

	// Start command asynchronously
	logger.V(2).Infof("Executing: %v \n", cmd.Args)
	if err := cmd.Start(); err != nil {
		return result, fmt.Errorf("cmd.Start: %w", err)
	}

	var timer *time.Timer
	// Create a timer that will kill the process
	if conf.Timeout != nil {
		timer = time.AfterFunc(*conf.Timeout, func() {
			cmd.Process.Kill()
		})
	}

	// Wait for execution
	result.ExitErr = cmd.Wait()

	// Populate the result object
	result.Stdout = outbuf.Bytes()
	result.Stderr = errbuf.Bytes()

	// when the execution times out return a timeout error
	if conf.Timeout != nil && !timer.Stop() {
		return result, &ExecError{
			errmsg:     ErrTimeout.Error(),
			procresult: result,
		}
	}

	result.ExitCode = cmd.ProcessState.ExitCode()

	if conf.Verifier != nil {
		return verify(path, result, *conf.Verifier)
	}

	return result, nil
}

// GetServiceState interrogates local system services and returns their status and configuration.
func GetServiceState(name string) (svc.Status, mgr.Config, error) {
	m, err := mgr.Connect()
	if err != nil {
		return svc.Status{}, mgr.Config{}, err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return svc.Status{}, mgr.Config{}, fmt.Errorf("could not access service: %v", err)
	}
	defer s.Close()

	config, err := s.Config()
	if err != nil {
		return svc.Status{}, mgr.Config{}, err
	}
	status, err := s.Query()
	return status, config, err
}

const (
	HWND_BROADCAST   = uintptr(0xffff)
	WM_SETTINGCHANGE = uintptr(0x001A)
)

// GetSysEnv gets a system environment variable
func GetSysEnv(key string) (string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `System\CurrentControlSet\Control\Session Manager\Environment`, registry.READ)
	if err != nil {
		return "", err
	}
	defer k.Close()
	v, _, err := k.GetStringValue(key)
	return v, err
}

// RestartService attempts to restart local system services.
func RestartService(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return err
	}
	defer s.Close()

	if err := stopService(s); err != nil {
		return err
	}

	return s.Start()
}

// SetSysEnv sets a system environment variable
func SetSysEnv(key, value string) error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `System\CurrentControlSet\Control\Session Manager\Environment`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.SetStringValue(key, value); err != nil {
		return err
	}

	// refresh existing windows
	r, _, err := syscall.NewLazyDLL("user32.dll").NewProc("SendMessageW").Call(HWND_BROADCAST, WM_SETTINGCHANGE, 0, uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr("ENVIRONMENT"))))
	if r != 1 {
		return fmt.Errorf("SendMessageW() exited with %q and error %v", r, err)
	}
	return nil
}

// StartService attempts to start local system services.
func StartService(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return err
	}
	defer s.Close()

	return s.Start()
}

func stopService(s *mgr.Service) error {
	stat, err := s.Control(svc.Stop)
	if err != nil {
		return err
	}
	retry := 0
	for stat.State != svc.Stopped {
		logger.Infof("Waiting for service %q to stop.", s.Name)
		time.Sleep(5 * time.Second)
		retry++
		if retry > 12 {
			return fmt.Errorf("timed out waiting for service %q to stop", s.Name)
		}
		stat, err = s.Query()
		if err != nil {
			return err
		}
	}
	return nil
}

// StopService attempts to stop local system services.
func StopService(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return err
	}
	defer s.Close()

	return stopService(s)
}

// WaitForProcessExit waits for a process to stop (no longer appear in the process list).
func WaitForProcessExit(matcher *regexp.Regexp, timeout time.Duration) error {
	t := time.NewTicker(timeout)
	defer t.Stop()
	r := time.NewTicker(5 * time.Second)
	defer r.Stop()

loop:
	for {
		select {
		case <-t.C:
			return ErrTimeout
		case <-r.C:
			procs, err := fnProcessList()
			if err != nil {
				return fmt.Errorf("winapi.ProcessList: %w", err)
			}
			for _, p := range procs {
				if matcher.MatchString(p.Executable) {
					logger.Warningf("Process %s still running; waiting for exit.", p.Executable)
					goto loop
				}
			}
			return nil
		}
	}
}

// ContainsString returns true if a string is in slice and false otherwise.
func ContainsString(a string, slice []string) bool {
	for _, b := range slice {
		if a == b {
			return true
		}
	}
	return false
}

// PathExists returns whether the given file or directory exists or not
func PathExists(path string) (bool, error) {
	if strings.TrimSpace(path) == "" {
		return false, errors.New("path cannot be empty")
	}

	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return false, nil
}

// StringToSlice converts a comma separated string to a slice.
func StringToSlice(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	a := strings.Split(s, ",")
	for i, item := range a {
		a[i] = strings.TrimSpace(item)
	}
	return a
}

// StringToMap converts a comma separated string to a map.
func StringToMap(s string) map[string]bool {
	m := map[string]bool{}
	if s != "" {
		for _, item := range strings.Split(s, ",") {
			m[strings.TrimSpace(item)] = true
		}
	}
	return m
}

// StringToPtrOrNil converts a non-empty string to a UTF16Ptr, but leaves a nil value for empty strings.
//
// This is primarily useful for Windows API calls where an "unset" parameter must be nil, and a pointer to
// an empty string would be considered invalid.
func StringToPtrOrNil(in string) (out *uint16) {
	if in != "" {
		out = windows.StringToUTF16Ptr(in)
	}
	return
}
