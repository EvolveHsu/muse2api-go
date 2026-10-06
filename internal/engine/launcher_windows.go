//go:build !unix

package engine

import (
	"os"
	"os/exec"
)

// execLauncher 的 Windows 分支：本机只用于跑测试，生产镜像跑在 Linux 上（见 launcher_unix.go）。
func execLauncher(args, env []string, dir string, lf *os.File) (Process, error) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Args = args
	cmd.Env = env
	cmd.Dir = dir
	cmd.Stdout = lf
	cmd.Stderr = lf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &execProcess{cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

type execProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func (p *execProcess) Alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *execProcess) Terminate() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(os.Interrupt)
	}
}

func (p *execProcess) Kill() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}
