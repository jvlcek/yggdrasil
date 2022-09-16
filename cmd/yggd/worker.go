package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"git.sr.ht/~spc/go-log"
	"github.com/pelletier/go-toml"
	"github.com/redhatinsights/yggdrasil"
	"github.com/redhatinsights/yggdrasil/internal/config"
	"github.com/rjeczalik/notify"
	"golang.org/x/net/http/httpproxy"
)

type workerConfig struct {
	Exec      string   `toml:"exec"`
	Protocol  string   `toml:"protocol"`
	Env       []string `toml:"env"`
	delay     time.Duration
	directive string
}

// loadWorkerConfig reads the contents of file and parses it into a workerConfig
// value.
func loadWorkerConfig(file string) (*workerConfig, error) {
	data, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("cannot read file: %w", err)
	}

	var worker workerConfig
	if err := toml.Unmarshal(data, &worker); err != nil {
		return nil, fmt.Errorf("cannot load config: %w", err)
	}
	worker.directive = strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))

	return &worker, nil
}

// startWorker constructs a command to execute from the given workerConfig,
// starts it, and starts a goroutine that waits for the process to exit. If not
// nil, started is invoked after the process is started. Likewise, when the
// process is stopped, stopped is invoked.
func startWorker(worker workerConfig, started func(pid int), stopped func(pid int)) error {
	argv := strings.Split(worker.Exec, " ")

	program := argv[0]
	var args []string
	if len(argv) > 1 {
		args = argv[1:]
	}

	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"YGG_CONFIG_DIR=" + filepath.Join(yggdrasil.SysconfDir, yggdrasil.LongName),
		"YGG_LOG_LEVEL=" + log.CurrentLevel().String(),
		"YGG_CLIENT_ID=" + config.DefaultConfig.ClientID,
	}

	proxy := httpproxy.FromEnvironment()
	if proxy.HTTPProxy != "" {
		env = append(env, "HTTP_PROXY="+proxy.HTTPProxy)
	}
	if proxy.HTTPSProxy != "" {
		env = append(env, "HTTPS_PROXY="+proxy.HTTPSProxy)
	}
	if proxy.NoProxy != "" {
		env = append(env, "NO_PROXY="+proxy.NoProxy)
	}

	switch worker.Protocol {
	case "grpc":
		env = append(env, "YGG_SOCKET_ADDR=unix:"+config.DefaultConfig.SocketAddr)
	default:
		return fmt.Errorf("unsupported protocol: %v", worker.Protocol)
	}

	for _, val := range worker.Env {
		if validEnvVar(val) {
			env = append(env, val)
		}
	}

	if worker.delay < 0 {
		return fmt.Errorf("failed to start worker %v too many times", program)
	}

	if worker.delay > 0 {
		log.Tracef("delaying worker start for %v...", worker.delay)
		time.Sleep(worker.delay)
	}

	err := startProcess(program, args, env, func(pid int, stdout, stderr io.ReadCloser) {
		go func() {
			for {
				buf := make([]byte, 4096)
				n, err := stdout.Read(buf)
				if n > 0 {
					log.Tracef("[%v] %v", program, strings.TrimRight(string(buf), "\n\x00"))
				}
				if err != nil {
					switch err {
					case io.EOF:
						log.Debugf("%v stdout reached EOF: %v", program, err)
						return
					default:
						log.Errorf("cannot read from stdout: %v", err)
						continue
					}
				}
			}
		}()

		go func() {
			for {
				buf := make([]byte, 4096)
				n, err := stderr.Read(buf)
				if n > 0 {
					log.Tracef("[%v] %v", program, strings.TrimRight(string(buf), "\n\x00"))
				}
				if err != nil {
					switch err {
					case io.EOF:
						log.Debugf("%v stderr reached EOF: %v", program, err)
						return
					default:
						log.Errorf("cannot read from stderr: %v", err)
						continue
					}
				}
			}
		}()

		pidDirPath := filepath.Join(yggdrasil.LocalstateDir, "run", yggdrasil.LongName, "workers")

		if err := os.MkdirAll(pidDirPath, 0755); err != nil {
			log.Errorf("cannot create directory: %v", err)
			return
		}

		if err := ioutil.WriteFile(filepath.Join(pidDirPath, worker.directive+".pid"), []byte(fmt.Sprintf("%v", pid)), 0644); err != nil {
			log.Errorf("cannot write to file: %v", err)
			return
		}

		if started != nil {
			go started(pid)
		}

		if err := waitProcess(pid, func(pid int, state *os.ProcessState) {
			log.Infof("worker stopped: %v", pid)

			if state.SystemTime() < time.Duration(1*time.Second) {
				worker.delay += 5 * time.Second
			}

			if worker.delay >= time.Duration(30*time.Second) {
				worker.delay = -1
			}

			if stopped != nil {
				go stopped(pid)
			}

			if workerExists(worker.directive) {
				if err := startWorker(worker, started, stopped); err != nil {
					log.Errorf("cannot restart worker: %v", err)
					return
				}
			}
		}); err != nil {
			log.Errorf("process exited with an error: %v", err)
		}
	})

	if err != nil {
		return fmt.Errorf("cannot start worker: %w", err)
	}

	return nil
}

// stopWorker looks for a PID file with the given name, parses it as a integer,
// assumes it is a process PID and stops the process.
func stopWorker(name string) error {
	pidFile := filepath.Join(yggdrasil.LocalstateDir, "run", yggdrasil.LongName, "workers", name+".pid")

	data, err := ioutil.ReadFile(pidFile)
	if err != nil {
		return fmt.Errorf("cannot read file: %w", err)
	}
	pid, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return fmt.Errorf("cannot parse data: %w", err)
	}
	if err := stopProcess(int(pid)); err != nil {
		return fmt.Errorf("cannot stop worker: %w", err)
	}
	if err := os.Remove(pidFile); err != nil {
		return fmt.Errorf("cannot remove file: %w", err)
	}
	return nil
}

// stopWorkers reads all pid files from the local state directory and attempts
// to stop the worker process.
func stopWorkers() error {
	dir := filepath.Join(yggdrasil.LocalstateDir, "run", yggdrasil.LongName, "workers")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("cannot create directory: %w", err)
	}
	infos, err := ioutil.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("cannot read contents of directory: %w", err)
	}

	for _, info := range infos {
		if strings.HasSuffix(info.Name(), ".pid") {
			if err := stopWorker(strings.TrimSuffix(info.Name(), ".pid")); err != nil {
				return fmt.Errorf("cannot stop worker: %w", err)
			}
		}
	}

	return nil
}

func watchWorkerDir(dir string, died chan int) {
	c := make(chan notify.EventInfo, 1)

	if err := notify.Watch(dir, c, notify.InCloseWrite, notify.InDelete, notify.InMovedFrom, notify.InMovedTo); err != nil {
		log.Errorf("cannot start notify watchpoint: %v", err)
		return
	}
	defer notify.Stop(c)

	for e := range c {
		log.Debugf("received inotify event %v", e.Event())
		switch e.Event() {
		case notify.InCloseWrite, notify.InMovedTo:
			log.Tracef("new worker detected: %v", e.Path())
			worker, err := loadWorkerConfig(e.Path())
			if err != nil {
				log.Errorf("cannot load worker config: %v", err)
			}
			if config.DefaultConfig.ExcludeWorkers[worker.directive] {
				log.Tracef("skipping excluded worker %v", worker.directive)
				continue
			}
			log.Debugf("starting worker: %v", worker.directive)
			go func() {
				if err := startWorker(*worker, nil, func(pid int) {
					died <- pid
				}); err != nil {
					log.Errorf("cannot start worker %v: %v", worker.directive, err)
					return
				}
			}()
		case notify.InDelete, notify.InMovedFrom:
			name := strings.TrimSuffix(filepath.Base(e.Path()), filepath.Ext(e.Path()))

			if err := stopWorker(name); err != nil {
				log.Errorf("cannot kill worker %v: %v", name, err)
				continue
			}
		}
	}
}

func workerExists(name string) bool {
	_, err := os.Stat(filepath.Join(yggdrasil.SysconfDir, yggdrasil.LongName, "workers", name+".toml"))
	return !os.IsNotExist(err)
}

func validEnvVar(val string) bool {
	for _, pattern := range []string{"PATH=.*", "YGG_.*=.*"} {
		r := regexp.MustCompile(pattern)
		if r.Match([]byte(val)) {
			return false
		}
	}

	return true
}