package daemon

import (
	"io"
	"os"
)

func (j *Job) outWriter() io.Writer {
	if j.stdout != nil {
		return j.stdout
	}
	return os.Stdout
}

func (j *Job) errWriter() io.Writer {
	if j.stderr != nil {
		return j.stderr
	}
	return os.Stderr
}

type streamHook struct {
	stdout, stderr io.Writer
	done           chan int
}

// SubmitStreaming starts a job whose output goes to the given writers and
// returns the job plus a channel that yields the exit code when it ends. It is
// how the remote exec path runs a client's command under a real GPU share and
// streams the result back. A queued job's channel fires later, when it gets a
// slot.
func (d *Daemon) SubmitStreaming(r SubmitRequest, stdout, stderr io.Writer) (*Job, <-chan int, error) {
	done := make(chan int, 1)
	d.mu.Lock()
	d.prepareStreaming = &streamHook{stdout: stdout, stderr: stderr, done: done}
	d.mu.Unlock()
	job, err := d.Submit(r)
	if err != nil {
		d.mu.Lock()
		d.prepareStreaming = nil
		d.mu.Unlock()
		return nil, nil, err
	}
	return job, done, nil
}
