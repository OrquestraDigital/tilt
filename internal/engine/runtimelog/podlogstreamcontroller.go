package runtimelog

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/google/go-cmp/cmp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/tilt-dev/tilt/internal/container"
	"github.com/tilt-dev/tilt/internal/k8s"
	"github.com/tilt-dev/tilt/internal/store"
	"github.com/tilt-dev/tilt/internal/store/k8sconv"
	"github.com/tilt-dev/tilt/pkg/logger"
)

var podLogHealthCheck = 15 * time.Second
var podLogReconnectGap = 2 * time.Second

// Reconciles the PodLogStream API object.
//
// Collects logs from deployed containers.
type PodLogStreamController struct {
	ctx       context.Context
	client    ctrlclient.Client
	st        store.RStore
	kClient   k8s.Client
	podSource *PodSource
	mu        sync.Mutex

	watches         map[podLogKey]PodLogWatch
	hasClosedStream map[podLogKey]bool
	statuses        map[types.NamespacedName]*PodLogStreamStatus

	newTicker func(d time.Duration) *time.Ticker
	since     func(t time.Time) time.Duration
	now       func() time.Time
}

var _ reconcile.Reconciler = &PodLogStreamController{}
var _ store.TearDowner = &PodLogStreamController{}

func NewPodLogStreamController(ctx context.Context, client ctrlclient.Client, st store.RStore, kClient k8s.Client) *PodLogStreamController {
	return &PodLogStreamController{
		ctx:             ctx,
		client:          client,
		st:              st,
		kClient:         kClient,
		podSource:       NewPodSource(ctx, kClient),
		watches:         make(map[podLogKey]PodLogWatch),
		hasClosedStream: make(map[podLogKey]bool),
		statuses:        make(map[types.NamespacedName]*PodLogStreamStatus),
		newTicker:       time.NewTicker,
		since:           time.Since,
		now:             time.Now,
	}
}

// Filter containers based on the inclusions/exclusions in the PodLogStream spec.
func (m *PodLogStreamController) filterContainers(stream *PodLogStream, containers []store.Container) []store.Container {
	if len(stream.Spec.OnlyContainers) > 0 {
		only := make(map[container.Name]bool, len(stream.Spec.OnlyContainers))
		for _, name := range stream.Spec.OnlyContainers {
			only[container.Name(name)] = true
		}

		result := []store.Container{}
		for _, c := range containers {
			if only[c.Name] {
				result = append(result, c)
			}
		}
		return result
	}

	if len(stream.Spec.IgnoreContainers) > 0 {
		ignore := make(map[container.Name]bool, len(stream.Spec.IgnoreContainers))
		for _, name := range stream.Spec.IgnoreContainers {
			ignore[container.Name(name)] = true
		}

		result := []store.Container{}
		for _, c := range containers {
			if !ignore[c.Name] {
				result = append(result, c)
			}
		}
		return result
	}
	return containers
}

func (c *PodLogStreamController) SetClient(client ctrlclient.Client) {
	c.client = client
}

func (c *PodLogStreamController) TearDown(ctx context.Context) {
	c.podSource.TearDown()
}

func (m *PodLogStreamController) shouldStreamContainerLogs(c store.Container, key podLogKey) bool {
	if c.ID == "" {
		return false
	}

	if c.Terminated && m.hasClosedStream[key] {
		return false
	}

	if !(c.Running || c.Terminated) {
		return false
	}

	return true

}

// Reconcile the given stream against what we're currently tracking.
func (r *PodLogStreamController) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	stream := &PodLogStream{}
	streamName := req.NamespacedName
	err := r.client.Get(ctx, req.NamespacedName, stream)
	if apierrors.IsNotFound(err) {
		r.podSource.handleReconcileRequest(ctx, req.NamespacedName, stream)
		r.deleteStreams(streamName)
		return reconcile.Result{}, nil
	} else if err != nil {
		return reconcile.Result{}, err
	}

	ctx = store.MustObjectLogHandler(ctx, r.st, stream)
	r.podSource.handleReconcileRequest(ctx, req.NamespacedName, stream)

	podNN := types.NamespacedName{Name: stream.Spec.Pod, Namespace: stream.Spec.Namespace}
	pod, err := r.kClient.PodFromInformerCache(ctx, podNN)
	if (err != nil && apierrors.IsNotFound(err)) ||
		(pod != nil && pod.DeletionTimestamp != nil && !pod.DeletionTimestamp.IsZero()) {
		r.deleteStreams(streamName)
		return reconcile.Result{}, nil
	} else if err != nil {
		logger.Get(ctx).Debugf("streaming logs: %v", err)
		return reconcile.Result{}, err
	} else if pod == nil {
		logger.Get(ctx).Debugf("streaming logs: pod not found: %s", podNN)
		return reconcile.Result{}, nil
	}

	initContainers := r.filterContainers(stream, k8sconv.PodContainers(ctx, pod, pod.Status.InitContainerStatuses))
	runContainers := r.filterContainers(stream, k8sconv.PodContainers(ctx, pod, pod.Status.ContainerStatuses))
	containers := []store.Container{}
	containers = append(containers, initContainers...)
	containers = append(containers, runContainers...)
	r.ensureStatus(streamName, containers)

	containerWatches := make(map[podLogKey]bool)
	for i, c := range containers {
		// Key the log watcher by the container id, so we auto-restart the
		// watching if the container crashes.
		key := podLogKey{
			streamName: streamName,
			podID:      k8s.PodID(podNN.Name),
			cID:        c.ID,
		}
		if !r.shouldStreamContainerLogs(c, key) {
			continue
		}

		isInitContainer := i < len(initContainers)

		// We don't want to clutter the logs with a container name
		// if it's unambiguous what container we're looking at.
		//
		// Long-term, we should make the container name a log field
		// and have better ways to display it visually.
		shouldPrefix := isInitContainer || len(runContainers) > 1

		containerWatches[key] = true

		existing, isActive := r.watches[key]

		// Only stream logs that have happened since Tilt started.
		//
		// TODO(nick): We should really record when we started the `kubectl apply`,
		// and only stream logs since that happened.
		startWatchTime := time.Time{}
		if stream.Spec.SinceTime != nil {
			startWatchTime = stream.Spec.SinceTime.Time
		}

		if isActive {
			if existing.ctx.Err() == nil {
				// The active pod watcher is still tailing the logs,
				// nothing to do.
				continue
			}

			// The active pod watcher got canceled somehow,
			// so we need to create a new one that picks up
			// where it left off.
			startWatchTime = <-existing.terminationTime
			r.hasClosedStream[key] = true
			if c.Terminated {
				r.mutateStatus(streamName, c.Name, func(cs *ContainerLogStreamStatus) {
					cs.Terminated = true
					cs.Active = false
					cs.Error = ""
				})
				continue
			}
		}

		ctx, cancel := context.WithCancel(ctx)
		w := PodLogWatch{
			streamName:      streamName,
			ctx:             ctx,
			cancel:          cancel,
			podID:           k8s.PodID(podNN.Name),
			cName:           c.Name,
			namespace:       k8s.Namespace(podNN.Namespace),
			startWatchTime:  startWatchTime,
			terminationTime: make(chan time.Time, 1),
			shouldPrefix:    shouldPrefix,
		}
		r.watches[key] = w

		go r.consumeLogs(w, r.st)
	}

	for key, watch := range r.watches {
		_, inState := containerWatches[key]
		if !inState && key.streamName == streamName {
			watch.cancel()
			delete(r.watches, key)
		}
	}

	r.updateStatus(streamName)

	return reconcile.Result{}, nil
}

// Delete all the streams generated by the named API object
func (c *PodLogStreamController) deleteStreams(streamName types.NamespacedName) {
	for k, watch := range c.watches {
		if k.streamName != streamName {
			continue
		}
		watch.cancel()
		delete(c.watches, k)
	}

	c.mu.Lock()
	delete(c.statuses, streamName)
	c.mu.Unlock()
}

func (m *PodLogStreamController) consumeLogs(watch PodLogWatch, st store.RStore) {
	defer func() {
		watch.terminationTime <- m.now()
		watch.cancel()
	}()

	pID := watch.podID
	containerName := watch.cName
	ns := watch.namespace
	startReadTime := watch.startWatchTime
	ctx := watch.ctx
	if watch.shouldPrefix {
		prefix := fmt.Sprintf("[%s] ", watch.cName)
		ctx = logger.WithLogger(ctx, logger.NewPrefixedLogger(prefix, logger.Get(ctx)))
	}

	retry := true
	for retry {
		retry = false
		ctx, cancel := context.WithCancel(ctx)
		readCloser, err := m.kClient.ContainerLogs(ctx, pID, containerName, ns, startReadTime)
		if err != nil {
			cancel()

			m.mutateStatus(watch.streamName, containerName, func(cs *ContainerLogStreamStatus) {
				cs.Active = false
				cs.Error = err.Error()
			})
			m.updateStatus(watch.streamName)

			// TODO(nick): Should this be Warnf/Errorf?
			logger.Get(ctx).Infof("Error streaming %s logs: %v", pID, err)
			return
		}

		reader := NewHardCancelReader(ctx, readCloser)
		reader.now = m.now

		// A hacky workaround for
		// https://github.com/tilt-dev/tilt/issues/3908
		// Every 15 seconds, check to see if the logs have stopped streaming.
		// If they have, reconnect to the log stream.
		done := make(chan bool)
		go func() {
			ticker := m.newTicker(podLogHealthCheck)
			for {
				select {
				case <-done:
					return

				case <-ticker.C:
					lastRead := reader.LastReadTime()
					if lastRead.IsZero() || m.since(lastRead) < podLogHealthCheck {
						continue
					}

					retry = true

					// Start reading 2 seconds after the last read.
					//
					// In the common case (where we just haven't gotten any logs in the
					// last 15 seconds), this will ensure we don't duplicate logs.
					//
					// In the uncommon case (where the Kuberentes log buffer exceeded 10MB
					// and got rotated), this will create a 2 second gap in the log, but
					// we think this is acceptable to avoid the duplicate case.
					startReadTime = lastRead.Add(podLogReconnectGap)
					cancel()
					return
				}
			}
		}()

		m.mutateStatus(watch.streamName, containerName, func(cs *ContainerLogStreamStatus) {
			cs.Active = true
			cs.Error = ""
		})
		m.updateStatus(watch.streamName)

		_, err = io.Copy(logger.Get(ctx).Writer(logger.InfoLvl), reader)
		_ = readCloser.Close()
		close(done)
		cancel()

		if !retry && err != nil && ctx.Err() == nil {
			m.mutateStatus(watch.streamName, containerName, func(cs *ContainerLogStreamStatus) {
				cs.Active = false
				cs.Error = err.Error()
			})
			m.updateStatus(watch.streamName)

			// TODO(nick): Should this be Warnf/Errorf?
			logger.Get(ctx).Infof("Error streaming %s logs: %v", pID, err)
			return
		}
	}
}

// Set up the status object for a particular stream, tracking each container individually.
func (r *PodLogStreamController) ensureStatus(streamName types.NamespacedName, containers []store.Container) {
	r.mu.Lock()
	defer r.mu.Unlock()

	status, ok := r.statuses[streamName]
	if !ok {
		status = &PodLogStreamStatus{}
		r.statuses[streamName] = status
	}

	// Make sure the container names are right. If they're not, delete everything and recreate.
	isMatching := len(containers) == len(status.ContainerStatuses)
	if isMatching {
		for i, cs := range status.ContainerStatuses {
			if string(containers[i].Name) != cs.Name {
				isMatching = false
				break
			}
		}
	}

	if isMatching {
		return
	}

	statuses := make([]ContainerLogStreamStatus, 0, len(containers))
	for _, c := range containers {
		statuses = append(statuses, ContainerLogStreamStatus{
			Name: string(c.Name),
		})
	}
	status.ContainerStatuses = statuses
}

// Modify the status of a container log stream.
func (r *PodLogStreamController) mutateStatus(streamName types.NamespacedName, containerName container.Name, mutate func(*ContainerLogStreamStatus)) {
	r.mu.Lock()
	defer r.mu.Unlock()

	status, ok := r.statuses[streamName]
	if !ok {
		return
	}

	for i, cs := range status.ContainerStatuses {
		if cs.Name != string(containerName) {
			continue
		}

		mutate(&cs)
		status.ContainerStatuses[i] = cs
	}
}

// Update the server with the current container status.
func (r *PodLogStreamController) updateStatus(streamName types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()

	status, ok := r.statuses[streamName]
	if !ok {
		return
	}

	stream := &PodLogStream{}
	err := r.client.Get(r.ctx, streamName, stream)
	if err != nil || cmp.Equal(stream.Status, status) {
		return
	}

	status.DeepCopyInto(&stream.Status)
	_ = r.client.Status().Update(r.ctx, stream)
}

func (c *PodLogStreamController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&PodLogStream{}).
		Watches(c.podSource, handler.Funcs{}).
		Complete(c)
}

type PodLogWatch struct {
	ctx    context.Context
	cancel func()

	streamName      types.NamespacedName
	podID           k8s.PodID
	namespace       k8s.Namespace
	cName           container.Name
	startWatchTime  time.Time
	terminationTime chan time.Time

	shouldPrefix bool // if true, we'll prefix logs with the container name
}

type podLogKey struct {
	streamName types.NamespacedName
	podID      k8s.PodID
	cID        container.ID
}
