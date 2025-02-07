package k8swatch

import (
	"context"
	"sync"
	"time"

	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/tilt-dev/tilt/internal/k8s"
	"github.com/tilt-dev/tilt/internal/store"
	"github.com/tilt-dev/tilt/pkg/logger"
	"github.com/tilt-dev/tilt/pkg/model"
)

// TODO(nick): Right now, the EventWatchManager, PodWatcher, and ServiceWatcher
// all look very similar, with a few subtle differences (particularly in how
// we decide whether two objects are related, and how we index those relationships).
//
// We're probably missing some abstractions here.
//
// TODO(nick): We should also add garbage collection and/or handle Delete events
// from the kubernetes informer properly.
type EventWatchManager struct {
	kClient      k8s.Client
	ownerFetcher k8s.OwnerFetcher

	mu                sync.RWMutex
	watcherKnownState watcherKnownState

	// An index that maps the UID of Kubernetes resources to the UIDs of
	// all events that they own (transitively).
	//
	// For example, a Deployment UID might contain a set of N event UIDs.
	knownDescendentEventUIDs map[types.UID]store.UIDSet

	// An index of all the known events, by UID
	knownEvents map[types.UID]*v1.Event
}

func NewEventWatchManager(kClient k8s.Client, ownerFetcher k8s.OwnerFetcher, cfgNS k8s.Namespace) *EventWatchManager {
	return &EventWatchManager{
		kClient:                  kClient,
		ownerFetcher:             ownerFetcher,
		watcherKnownState:        newWatcherKnownState(cfgNS),
		knownDescendentEventUIDs: make(map[types.UID]store.UIDSet),
		knownEvents:              make(map[types.UID]*v1.Event),
	}
}

type eventWatchTaskList struct {
	watcherTaskList
	tiltStartTime time.Time
}

func (m *EventWatchManager) diff(st store.RStore) eventWatchTaskList {
	state := st.RLockState()
	defer st.RUnlockState()

	m.mu.RLock()
	defer m.mu.RUnlock()

	watcherTaskList := m.watcherKnownState.createTaskList(state)
	return eventWatchTaskList{
		watcherTaskList: watcherTaskList,
		tiltStartTime:   state.TiltStartTime,
	}
}

func (m *EventWatchManager) OnChange(ctx context.Context, st store.RStore, _ store.ChangeSummary) {
	taskList := m.diff(st)

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, teardown := range taskList.teardownNamespaces {
		watcher, ok := m.watcherKnownState.namespaceWatches[teardown]
		if ok {
			watcher.cancel()
		}
		delete(m.watcherKnownState.namespaceWatches, teardown)
	}

	for _, setup := range taskList.setupNamespaces {
		m.setupWatch(ctx, st, setup, taskList.tiltStartTime)
	}

	if len(taskList.newUIDs) > 0 {
		m.setupNewUIDs(ctx, st, taskList.newUIDs)
	}
}

func (m *EventWatchManager) setupWatch(ctx context.Context, st store.RStore, ns k8s.Namespace, tiltStartTime time.Time) {
	ch, err := m.kClient.WatchEvents(ctx, ns)
	if err != nil {
		err = errors.Wrapf(err, "Error watching events. Are you connected to kubernetes?\nTry running `kubectl get events -n %q`", ns)
		st.Dispatch(store.NewErrorAction(err))
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	m.watcherKnownState.namespaceWatches[ns] = namespaceWatch{cancel: cancel}

	go m.dispatchEventsLoop(ctx, ch, st, tiltStartTime)
}

// When new UIDs are deployed, go through all our known events and dispatch
// new actions. This handles the case where we get the event
// before the deploy id shows up in the manifest, which is way more common than
// you would think.
func (m *EventWatchManager) setupNewUIDs(ctx context.Context, st store.RStore, newUIDs map[types.UID]model.ManifestName) {
	for uid, mn := range newUIDs {
		m.watcherKnownState.knownDeployedUIDs[uid] = mn

		descendants := m.knownDescendentEventUIDs[uid]
		for uid := range descendants {
			event, ok := m.knownEvents[uid]
			if ok {
				st.Dispatch(store.NewK8sEventAction(event, mn))
			}
		}
	}
}

// Check to see if this event corresponds to any of our manifests.
//
// We do this by comparing the event's InvolvedObject UID and its owner UIDs
// against what we've deployed to the cluster. Returns the ManifestName that it
// matched against.
//
// If the event doesn't match an existing deployed resource, keep it in local
// state, so we can match it later if the owner UID shows up.
func (m *EventWatchManager) triageEventUpdate(event *v1.Event, objTree k8s.ObjectRefTree) model.ManifestName {
	m.mu.Lock()
	defer m.mu.Unlock()

	uid := event.UID
	m.knownEvents[uid] = event

	// Set up the descendent index of the involved object
	for _, ownerUID := range objTree.UIDs() {
		set, ok := m.knownDescendentEventUIDs[ownerUID]
		if !ok {
			set = store.NewUIDSet()
			m.knownDescendentEventUIDs[ownerUID] = set
		}
		set.Add(uid)
	}

	// Find the manifest name
	for _, ownerUID := range objTree.UIDs() {
		mn, ok := m.watcherKnownState.knownDeployedUIDs[ownerUID]
		if ok {
			return mn
		}
	}

	return ""
}

func (m *EventWatchManager) dispatchEventChange(ctx context.Context, event *v1.Event, st store.RStore) {
	objTree, err := m.ownerFetcher.OwnerTreeOfRef(ctx, event.InvolvedObject)
	if err != nil {
		logger.Get(ctx).Infof("Error handling event update (%q): %v", event.Name, err)
		return
	}

	mn := m.triageEventUpdate(event, objTree)
	if mn == "" {
		return
	}

	st.Dispatch(store.NewK8sEventAction(event, mn))
}

func (m *EventWatchManager) dispatchEventsLoop(ctx context.Context, ch <-chan *v1.Event, st store.RStore, tiltStartTime time.Time) {
	for {
		select {
		case event, ok := <-ch:
			if !ok {
				return
			}

			// on startup, k8s will give us a bunch of event objects that happened
			// before tilt started, which leads to flooding the k8s api with lookups
			// on those events' involvedObjects we don't care about those events, so
			// ignore them.
			//
			// TODO(nick): We might need to remove this check and optimize
			// it in a different way. We want Tilt to be to attach to existing
			// resources, and these resources might have pre-existing events.
			if event.ObjectMeta.CreationTimestamp.Time.Before(tiltStartTime) {
				continue
			}

			if !ShouldLogEvent(event) {
				continue
			}

			go m.dispatchEventChange(ctx, event, st)

		case <-ctx.Done():
			return
		}
	}
}

const ImagePullingReason = "Pulling"
const ImagePulledReason = "Pulled"

func ShouldLogEvent(e *v1.Event) bool {
	if e.Type != v1.EventTypeNormal {
		return true
	}

	return e.Reason == ImagePullingReason || e.Reason == ImagePulledReason
}
