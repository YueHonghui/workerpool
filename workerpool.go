package workerpool

import "time"

const (
	// Size of queue to which tasks are submitted.  This can be small, no
	// matter how many tasks are submitted, because the dispatcher removes
	// tasks from this queue, scheduling each immediately to a ready worker, or
	// to a goroutine that will give it to the next ready worker.
	//
	// This value is also the size of the queue that workers register their
	// availability to the dispatcher.  There may be thousands of workers, but
	// only a small channel is needed to register some of the workers.
	//
	// While 0 (unbuffered) is usable, testing shows that a small amount of
	// buffering has slightly better performance with input bursts.
	taskQueueSize = 16

	// If worker pool receives no new work for this period of time, then stop
	// a worker goroutine.
	idleTimeoutSec = 5
)

type WorkerPool interface {
	// Submit enqueues a function for a worker to execute.
	//
	// Any external values needed by the task function must be captured in a
	// closure.  Any return values should be returned over a channel that is
	// captured in the task function closure.
	//
	// Submit will not block regardless of the number of tasks submitted.  Each
	// task is immediately given to an available worker or passed to a
	// goroutine to be given to the next available worker.  If there are no
	// available workers, the dispatcher adds a worker, until the maximum
	// number of workers is running.
	Submit(task func())

	// Stop stops the worker pool and waits for workers to complete.
	//
	// Since creating the worker pool starts at least one goroutine, for the
	// dispatcher, this function should be called when the worker pool is no
	// longer needed.
	Stop()

	// Stopped returns true if this worker pool has been stopped.
	Stopped() bool
}

// New creates and starts a pool of worker goroutines.
//
// The maxWorkers parameter specifies the maximum number of workers that will
// execute tasks concurrently.  After each timeout period, a worker goroutine
// is stopped until there are no remaining workers.
func New(maxWorkers int) WorkerPool {
	// There must be at least one worker.
	if maxWorkers < 1 {
		maxWorkers = 1
	}

	pool := &workerPool{
		taskQueue:    make(chan func(), taskQueueSize),
		maxWorkers:   maxWorkers,
		readyWorkers: make(chan chan func(), taskQueueSize),
		timeout:      time.Second * idleTimeoutSec,
		stoppedChan:  make(chan struct{}),
	}

	// Start the task dispatcher.
	go pool.dispatch()

	return pool
}

type workerPool struct {
	maxWorkers   int
	timeout      time.Duration
	taskQueue    chan func()
	readyWorkers chan chan func()
	stoppedChan  chan struct{}
}

// Stop stops the worker pool and waits for workers to complete.
func (p *workerPool) Stop() {
	if p.Stopped() {
		return
	}
	close(p.taskQueue)
	<-p.stoppedChan
}

// Stopped returns true if this worker pool has been stopped.
func (p *workerPool) Stopped() bool {
	select {
	case <-p.stoppedChan:
		return true
	default:
	}
	return false
}

// Submit enqueues a function for a worker to execute.
func (p *workerPool) Submit(task func()) {
	if task != nil {
		p.taskQueue <- task
	}
}

// dispatch sends the next queued task to an available worker.
func (p *workerPool) dispatch() {
	defer close(p.stoppedChan)
	timeout := time.NewTimer(p.timeout)
	var workerCount int
	var task func()
	var ok bool
	var workerTaskChan chan func()
	startReady := make(chan chan func())
Loop:
	for {
		timeout.Reset(p.timeout)
		select {
		case task, ok = <-p.taskQueue:
			if !ok {
				break Loop
			}
			// Got a task to do.
			select {
			case workerTaskChan = <-p.readyWorkers:
				// A worker is ready, so give task to worker.
				workerTaskChan <- task
			default:
				// No workers ready.
				// Create a new worker, if not at max.
				if workerCount < p.maxWorkers {
					workerCount++
					go func(t func()) {
						startWorker(startReady, p.readyWorkers)
						// Submit the task when the new worker.
						taskChan := <-startReady
						taskChan <- t
					}(task)
				} else {
					// Start a goroutine to submit the task when an existing
					// worker is ready.
					go func(t func()) {
						taskChan := <-p.readyWorkers
						taskChan <- t
					}(task)
				}
			}
		case <-timeout.C:
			// Timed out waiting for work to arrive.  Kill a ready worker.
			if workerCount > 0 {
				select {
				case workerTaskChan = <-p.readyWorkers:
					// A worker is ready, so kill.
					close(workerTaskChan)
					workerCount--
				default:
					// No work, but no ready workers.  All workers are busy.
				}
			}
		}
	}

	// Stop all remaining workers as they become ready.
	for workerCount > 0 {
		workerTaskChan = <-p.readyWorkers
		close(workerTaskChan)
		workerCount--
	}
}

// startWorker starts a goroutine that executes tasks given by the dispatcher.
//
// When a new worker starts, it registers its availability on the startReady
// channel.  This ensures that the goroutine associated with starting the
// worker gets to use the worker to execute its task.  Otherwise, the main
// dispatcher loop could steal the new worker and not know to start up another
// worker for the waiting goroutine.  The task would then have to wait for
// another existing worker to become available, even though capacity is
// available to start additional workers.
//
// A worker registers that is it available to do work by putting its task
// channel on the readyWorkers channel.  The dispatcher reads a worker's task
// channel from the readyWorkers channel, and writes a task to the worker over
// the worker's task channel.  To stop a worker, the dispatcher closes a
// worker's task channel, instead of writing a task to it.
func startWorker(startReady, readyWorkers chan chan func()) {
	go func() {
		taskChan := make(chan func())
		var task func()
		var ok bool
		// Register availability on starReady channel.
		startReady <- taskChan
		for {
			// Read task from dispatcher.
			task, ok = <-taskChan
			if !ok {
				// Dispatcher has told worker to stop.
				break
			}

			// Execute the task.
			task()

			// Register availability on readyWorkers channel.
			readyWorkers <- taskChan
		}
	}()
}
