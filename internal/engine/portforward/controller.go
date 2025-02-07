package portforward

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/google/go-cmp/cmp"

	"github.com/tilt-dev/tilt/internal/engine/runtimelog"
	"github.com/tilt-dev/tilt/internal/k8s"
	"github.com/tilt-dev/tilt/internal/store"
	"github.com/tilt-dev/tilt/pkg/logger"
	"github.com/tilt-dev/tilt/pkg/model"
)

type Controller struct {
	kClient k8s.Client

	activeForwards map[k8s.PodID]portForwardEntry
}

func NewController(kClient k8s.Client) *Controller {
	return &Controller{
		kClient:        kClient,
		activeForwards: make(map[k8s.PodID]portForwardEntry),
	}
}

// Figure out the diff between what's in the data store and
// what port-forwarding is currently active.
func (m *Controller) diff(ctx context.Context, st store.RStore) (toStart []portForwardEntry, toShutdown []portForwardEntry) {
	state := st.RLockState()
	defer st.RUnlockState()

	statePods := make(map[k8s.PodID]bool, len(state.ManifestTargets))

	// Find all the port-forwards that need to be created.
	for _, mt := range state.Targets() {
		ms := mt.State
		manifest := mt.Manifest
		pod := ms.MostRecentPod()
		podID := pod.PodID
		if podID == "" {
			continue
		}

		// Only do port-forwarding if the pod is running.
		if pod.Phase != v1.PodRunning && !pod.Deleting {
			continue
		}

		forwards := populatePortForwards(manifest, pod)
		if len(forwards) == 0 {
			continue
		}

		statePods[podID] = true

		oldEntry, isActive := m.activeForwards[podID]
		if isActive {
			if cmp.Equal(oldEntry.forwards, forwards, cmp.AllowUnexported(model.PortForward{})) {
				continue
			}
			toShutdown = append(toShutdown, oldEntry)
		}

		ctx, cancel := context.WithCancel(ctx)
		entry := portForwardEntry{
			podID:     podID,
			name:      ms.Name,
			namespace: pod.Namespace,
			forwards:  forwards,
			ctx:       ctx,
			cancel:    cancel,
		}

		toStart = append(toStart, entry)
		m.activeForwards[podID] = entry
	}

	// Find all the port-forwards that aren't in the manifest anymore
	// and need to be shut down.
	for key, value := range m.activeForwards {
		_, inState := statePods[key]
		if inState {
			continue
		}

		toShutdown = append(toShutdown, value)
		delete(m.activeForwards, key)
	}

	return toStart, toShutdown
}

func (m *Controller) OnChange(ctx context.Context, st store.RStore, _ store.ChangeSummary) {
	toStart, toShutdown := m.diff(ctx, st)
	for _, entry := range toShutdown {
		entry.cancel()
	}

	for _, entry := range toStart {
		// Treat port-forwarding errors as part of the pod log
		ctx := logger.CtxWithLogHandler(entry.ctx, PodLogActionWriter{
			Store:        st,
			PodID:        entry.podID,
			ManifestName: entry.name,
		})

		for _, forward := range entry.forwards {
			entry := entry
			forward := forward
			go m.startPortForwardLoop(ctx, entry, forward)
		}
	}
}

func (m *Controller) startPortForwardLoop(ctx context.Context, entry portForwardEntry, forward model.PortForward) {
	originalBackoff := wait.Backoff{
		Steps:    1000,
		Duration: 50 * time.Millisecond,
		Factor:   2.0,
		Jitter:   0.1,
		Cap:      15 * time.Second,
	}
	currentBackoff := originalBackoff

	for {
		start := time.Now()
		err := m.onePortForward(ctx, entry, forward)
		if ctx.Err() != nil {
			// If the context was canceled, we're satisfied.
			// Ignore any errors.
			return
		}

		// Otherwise, repeat the loop, maybe logging the error
		if err != nil {
			logger.Get(ctx).Infof("Reconnecting... Error port-forwarding %s: %v", entry.name, err)
		}

		// If this failed in less than a second, then we should advance the backoff.
		// Otherwise, reset the backoff.
		if time.Since(start) < time.Second {
			time.Sleep(currentBackoff.Step())
		} else {
			currentBackoff = originalBackoff
		}
	}
}

func (m *Controller) onePortForward(ctx context.Context, entry portForwardEntry, forward model.PortForward) error {
	ns := entry.namespace
	podID := entry.podID

	pf, err := m.kClient.CreatePortForwarder(ctx, ns, podID, forward.LocalPort, forward.ContainerPort, forward.Host)
	if err != nil {
		return err
	}

	err = pf.ForwardPorts()
	if err != nil {
		return err
	}
	return nil
}

var _ store.Subscriber = &Controller{}

type portForwardEntry struct {
	name      model.ManifestName
	namespace k8s.Namespace
	podID     k8s.PodID
	forwards  []model.PortForward
	ctx       context.Context
	cancel    func()
}

// Extract the port-forward specs from the manifest. If any of them
// have ContainerPort = 0, populate them with the default port for the pod.
// Quietly drop forwards that we can't populate.
func populatePortForwards(m model.Manifest, pod store.Pod) []model.PortForward {
	cPorts := pod.AllContainerPorts()
	fwds := m.K8sTarget().PortForwards
	forwards := make([]model.PortForward, 0, len(fwds))
	for _, forward := range fwds {
		if forward.ContainerPort == 0 {
			if len(cPorts) == 0 {
				continue
			}

			forward.ContainerPort = int(cPorts[0])
			for _, cPort := range cPorts {
				if int(forward.LocalPort) == int(cPort) {
					forward.ContainerPort = int(cPort)
					break
				}
			}
		}
		forwards = append(forwards, forward)
	}
	return forwards
}

func PortForwardsAreValid(m model.Manifest, pod store.Pod) bool {
	expectedFwds := m.K8sTarget().PortForwards
	actualFwds := populatePortForwards(m, pod)
	return len(actualFwds) == len(expectedFwds)
}

type PodLogActionWriter struct {
	Store        store.RStore
	PodID        k8s.PodID
	ManifestName model.ManifestName
}

func (w PodLogActionWriter) Write(level logger.Level, fields logger.Fields, p []byte) error {
	w.Store.Dispatch(store.NewLogAction(w.ManifestName, runtimelog.SpanIDForPod(w.PodID), level, fields, p))
	return nil
}
