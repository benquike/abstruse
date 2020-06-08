package app

import (
	"context"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/jkuri/abstruse/internal/pkg/shared"
	"github.com/jkuri/abstruse/internal/pkg/util"
	"github.com/jkuri/abstruse/internal/server/db/model"
	"go.etcd.io/etcd/clientv3"
	recipe "go.etcd.io/etcd/contrib/recipes"
	"go.etcd.io/etcd/mvcc/mvccpb"
	"go.uber.org/zap"
)

type jobChannel chan shared.Job
type jobQueue chan chan shared.Job

type scheduler struct {
	mu      sync.Mutex
	workch  jobChannel
	workerq jobQueue
	queue   *recipe.PriorityQueue
	app     *App
	client  *clientv3.Client
	logger  *zap.SugaredLogger
	ready   chan struct{}
	max     int
	running int
	pending map[uint]*shared.Job
}

func newScheduler(client *clientv3.Client, logger *zap.Logger, app *App) *scheduler {
	return &scheduler{
		workch:  make(jobChannel),
		workerq: make(jobQueue),
		queue:   recipe.NewPriorityQueue(client, shared.QueuePrefix),
		app:     app,
		client:  client,
		logger:  logger.With(zap.String("type", "scheduler")).Sugar(),
		ready:   make(chan struct{}),
		pending: make(map[uint]*shared.Job),
	}
}

func (s *scheduler) run() error {
	wch, wdch := s.watchWorkers()
	donech := s.watchDoneEvents()
	queuech := s.dequeue()

	for {
		select {
		case job := <-s.workch:
			jobch := <-s.workerq
			jobch <- job
		case job := <-donech:
			go s.doneJob(job)
		case wid := <-wch:
			if worker, ok := s.app.workers[wid]; ok {
				worker.start(s.workerq)
			}
		case wid := <-wdch:
			if worker, ok := s.app.workers[wid]; ok {
				worker.stop()
			}
		case job := <-queuech:
			s.logger.Debugf("%+v", job)
			// go s.startJob(job)
		}
	}
}

// func (s *scheduler) scheduleJob(job shared.Job) {
// 	s.workch <- job
// }

// type scheduler struct {
// 	mu           sync.Mutex
// 	m            map[string]*concurrency.Mutex
// 	client       *clientv3.Client
// 	session      *concurrency.Session
// 	queue        *recipe.PriorityQueue
// 	logger       *zap.SugaredLogger
// 	pending      map[uint]shared.Job
// 	app          *App
// 	readych      chan struct{}
// 	max, running int
// 	wdch         chan string
// }

// func newScheduler(client *clientv3.Client, logger *zap.Logger, app *App) (*scheduler, error) {
// 	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
// 	defer cancel()
// 	session, err := concurrency.NewSession(client, concurrency.WithContext(ctx))
// 	if err != nil {
// 		return nil, err
// 	}
// 	return &scheduler{
// 		client:  client,
// 		session: session,
// 		queue:   recipe.NewPriorityQueue(client, shared.QueuePrefix),
// 		logger:  logger.With(zap.String("type", "scheduler")).Sugar(),
// 		pending: make(map[uint]shared.Job),
// 		m:       make(map[string]*concurrency.Mutex),
// 		app:     app,
// 		readych: make(chan struct{}, 1),
// 		wdch:    make(chan string),
// 	}, nil
// }

// func (s *scheduler) run() error {
// 	errch := make(chan error)
// 	donech := s.watchDoneEvents()
// 	wch, wdch := s.watchWorkers()
// 	queuech := s.dequeue()
// 	defer s.session.Close()

// 	go func() {
// 		for {
// 			select {
// 			case job := <-donech:
// 				go func(job shared.Job) {
// 					if err := s.doneJob(job); err != nil {
// 						errch <- err
// 					}
// 				}(job)
// 			}
// 		}
// 	}()

// 	go func() {
// 		for {
// 		loop:
// 			worker := <-wch
// 			select {
// 			case s.readych <- struct{}{}:
// 			default:
// 			}
// 			select {
// 			case job := <-queuech:
// 				go func(job shared.Job, worker string) {
// 					if err := s.startJob(job, worker); err != nil {
// 						errch <- err
// 					}
// 				}(job, worker)
// 			case wid := <-wdch:
// 				if worker == wid {
// 					goto loop
// 				}
// 			}
// 		}
// 	}()

// 	return <-errch
// }

func (s *scheduler) scheduleJob(job *shared.Job) error {
	s.logger.Infof("scheduling job %d with priority %d", job.ID, job.Priority)
	job.Status = shared.StatusQueued
	job.StartTime = nil
	job.EndTime = nil
	if err := s.save(job); err != nil {
		return err
	}
	data, err := job.Marshal()
	if err != nil {
		return err
	}
	return s.queue.Enqueue(data, uint16(job.Priority))
}

func (s *scheduler) stopJob(id uint) error {
	s.logger.Debugf("stopping job %d...", id)
	if job, ok := s.pending[id]; ok {
		job.EndTime = util.TimeNow()
		job.Status = shared.StatusFailing
		return s.put(shared.StopPrefix, job)
	}
	if job, err := s.deleteFromQueue(id); err == nil {
		job.StartTime = util.TimeNow()
		job.EndTime = util.TimeNow()
		job.Status = shared.StatusFailing
		return s.doneJob(job)
	}
	return nil
}

func (s *scheduler) setSize(max, running int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.max, s.running = max, running
	s.logger.Debugf("capacity: [%d / %d]", s.running, s.max)
}

func (s *scheduler) doneJob(job *shared.Job) error {
	s.logger.Infof("job %d done with status: %s", job.ID, job.GetStatus())
	if err := s.save(job); err != nil {
		s.logger.Errorf("error saving job to db %d: %v", job.ID, err)
		return err
	}
	if err := s.delete(shared.PendingPrefix, job); err != nil {
		s.logger.Errorf("error deleting job %d from pending: %v", job.ID, err)
		return err
	}
	if err := s.delete(shared.DonePrefix, job); err != nil {
		s.logger.Errorf("error deleting job %d from done: %v", job.ID, err)
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, job.ID)
	return nil
}

func (s *scheduler) startJob(job *shared.Job, worker string) error {
	s.logger.Debugf("job %d enqueued, sending job to worker %s", job.ID, worker)
	job.WorkerID = worker
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[job.ID] = job
	// TODO: move this to worker?
	job.Status = shared.StatusRunning
	job.StartTime = func(t time.Time) *time.Time { return &t }(time.Now())
	if err := s.save(job); err != nil {
		s.logger.Errorf("error saving job %d to db: %v", job.ID, err)
		return err
	}
	if err := s.put(shared.PendingPrefix, job); err != nil {
		s.logger.Errorf("error saving job %d to pending: %v", job.ID, err)
		return err
	}
	return nil
}

func (s *scheduler) dequeue() <-chan *shared.Job {
	ch := make(chan *shared.Job)

	go func() {
		for {
			<-s.ready
			if data, err := s.queue.Dequeue(); err == nil {
				job := &shared.Job{}
				if job, err := job.Unmarshal(data); err == nil {
					ch <- job
				}
			}
		}
	}()

	return ch
}

func (s *scheduler) watchDoneEvents() <-chan *shared.Job {
	donech := make(chan *shared.Job)

	go func() {
		resp, err := s.client.Get(context.Background(), shared.DonePrefix, clientv3.WithPrefix())
		if err != nil {
			s.logger.Errorf("error getting done jobs: %v", err)
		} else {
			for i := range resp.Kvs {
				job := &shared.Job{}
				job, err := job.Unmarshal(string(resp.Kvs[i].Value))
				if err != nil {
					s.logger.Errorf("error unmarshaling done job: %v", err)
				} else {
					donech <- job
				}
			}
		}

		wch := s.client.Watch(context.Background(), shared.DonePrefix, clientv3.WithPrefix())
		for n := range wch {
			for _, ev := range n.Events {
				switch ev.Type {
				case mvccpb.PUT:
					job := &shared.Job{}
					job, err := job.Unmarshal(string(ev.Kv.Value))
					if err != nil {
						s.logger.Errorf("%v", err)
					}
					donech <- job
				}
			}
		}
	}()

	return donech
}

func (s *scheduler) watchWorkers() (<-chan string, <-chan string) {
	putch, delch := make(chan string), make(chan string)

	go func() {
		resp, err := s.client.Get(context.Background(), shared.WorkersCapacity, clientv3.WithPrefix())
		if err != nil {
			s.logger.Errorf("error getting workers info: %v", err)
		} else {
			for i := range resp.Kvs {
				workerID := path.Base(string(resp.Kvs[i].Key))
				putch <- workerID
			}

			wch := s.client.Watch(context.Background(), shared.WorkersCapacity, clientv3.WithPrefix())
			for n := range wch {
				for _, ev := range n.Events {
					switch ev.Type {
					case mvccpb.PUT:
						workerID := path.Base(string(ev.Kv.Key))
						putch <- workerID
					case mvccpb.DELETE:
						delch <- path.Base(string(ev.Kv.Key))
					}
				}
			}
		}
	}()

	return putch, delch
}

// func (s *scheduler) listJobs(prefix string) ([]shared.Job, error) {
// 	var jobs []shared.Job
// 	resp, err := s.client.Get(context.Background(), prefix, clientv3.WithPrefix())
// 	if err != nil {
// 		return jobs, err
// 	}
// 	for i := range resp.Kvs {
// 		job := shared.Job{}
// 		job, err := job.Unmarshal(string(resp.Kvs[i].Value))
// 		if err != nil {
// 			return jobs, err
// 		}
// 		jobs = append(jobs, job)
// 	}
// 	return jobs, nil
// }

func (s *scheduler) deleteFromQueue(id uint) (*shared.Job, error) {
	job := &shared.Job{}
	resp, err := s.client.Get(context.Background(), shared.QueuePrefix, clientv3.WithPrefix())
	if err != nil {
		return job, err
	}
	for i := range resp.Kvs {
		job := &shared.Job{}
		job, err := job.Unmarshal(string(resp.Kvs[i].Value))
		if err != nil {
			return job, err
		}
		if job.ID == id {
			_, err := s.client.Delete(context.Background(), string(resp.Kvs[i].Key))
			return job, err
		}
	}
	return job, fmt.Errorf("not found")
}

func (s *scheduler) delete(prefix string, job *shared.Job) error {
	key := path.Join(prefix, fmt.Sprintf("%d", job.ID))
	_, err := s.client.Delete(context.Background(), key)
	return err
}

func (s *scheduler) put(prefix string, job *shared.Job) error {
	key := path.Join(prefix, fmt.Sprintf("%d", job.ID))
	val, err := job.Marshal()
	if err != nil {
		return err
	}
	_, err = s.client.Put(context.Background(), key, val)
	return err
}

func (s *scheduler) save(job *shared.Job) error {
	jobModel := &model.Job{
		ID:        job.ID,
		Status:    job.GetStatus(),
		StartTime: job.StartTime,
		EndTime:   job.EndTime,
		Log:       strings.Join(job.Log, ""),
	}
	_, err := s.app.jobRepository.Update(jobModel)
	if err != nil {
		return err
	}
	go s.broadcastJobStatus(job)
	return s.app.updateBuildTime(job.BuildID)
}

func (s *scheduler) broadcastJobStatus(job *shared.Job) {
	sub := path.Join("/subs", "jobs", fmt.Sprintf("%d", job.BuildID))
	event := map[string]interface{}{
		"build_id": job.BuildID,
		"job_id":   job.ID,
		"status":   job.GetStatus(),
	}
	if job.StartTime != nil {
		event["start_time"] = util.FormatTime(*job.StartTime)
	}
	if job.EndTime != nil {
		event["end_time"] = util.FormatTime(*job.EndTime)
	}
	s.app.ws.Broadcast(sub, event)
}
