//go:build unix

package engine

import (
	"os"
	"os/exec"
	"syscall"
)

// execLauncher 对应 subprocess.Popen(args, stdout=log, stderr=STDOUT, env=..., cwd=...)。
func execLauncher(args, env []string, dir string, lf *os.File) (Process, error) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Args = args // argv[0] 保持配置里的原样，与 Python 的 args 列表一致
	cmd.Env = env
	cmd.Dir = dir
	cmd.Stdout = lf
	cmd.Stderr = lf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &execProcess{cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait() // 主动 reap，避免跨重启累积僵尸进程
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

// Terminate 对应 proc.terminate()，即 SIGTERM。
func (p *execProcess) Terminate() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
	}
}

// Kill 对应 proc.kill()。
func (p *execProcess) Kill() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}
