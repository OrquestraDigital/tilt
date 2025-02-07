package engine

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"

	"github.com/tilt-dev/tilt/internal/container"
	"github.com/tilt-dev/tilt/internal/engine/k8swatch"
	"github.com/tilt-dev/tilt/internal/engine/portforward"
	"github.com/tilt-dev/tilt/internal/engine/runtimelog"
	"github.com/tilt-dev/tilt/internal/k8s"
	"github.com/tilt-dev/tilt/internal/store"
	"github.com/tilt-dev/tilt/internal/store/k8sconv"
	"github.com/tilt-dev/tilt/pkg/logger"
)

func handlePodDeleteAction(ctx context.Context, state *store.EngineState, action k8swatch.PodDeleteAction) {
	// PodDeleteActions only have the pod id. We don't have a good way to tie them back to their ancestors.
	// So just brute-force it.
	for _, target := range state.ManifestTargets {
		ms := target.State
		runtime := ms.K8sRuntimeState()
		delete(runtime.Pods, action.PodID)
	}
}

func handlePodChangeAction(ctx context.Context, state *store.EngineState, action k8swatch.PodChangeAction) {
	mt := matchPodChangeToManifest(state, action)
	if mt == nil {
		return
	}

	pod := action.Pod
	ms := mt.State
	manifest := mt.Manifest
	podInfo, isNew := maybeTrackPod(ms, action)
	if podInfo == nil {
		// This is an event from an old pod that has never been tracked.
		return
	}

	// Update the status
	podInfo.StartedAt = pod.CreationTimestamp.Time
	podInfo.Status = k8swatch.PodStatusToString(*pod)
	podInfo.Namespace = k8s.NamespaceFromPod(pod)
	podInfo.SpanID = runtimelog.SpanIDForPod(podInfo.PodID)
	podInfo.Deleting = pod.DeletionTimestamp != nil && !pod.DeletionTimestamp.IsZero()
	podInfo.Phase = pod.Status.Phase
	podInfo.StatusMessages = k8swatch.PodStatusErrorMessages(*pod)
	podInfo.Conditions = pod.Status.Conditions

	prunePods(ms)

	initContainers := k8sconv.PodContainers(ctx, pod, pod.Status.InitContainerStatuses)
	if !isNew {
		names := restartedContainerNames(podInfo.InitContainers, initContainers)
		for _, name := range names {
			s := fmt.Sprintf("Detected container restart. Pod: %s. Container: %s.", podInfo.PodID, name)
			handleLogAction(state, store.NewLogAction(manifest.Name, podInfo.SpanID, logger.WarnLvl, nil, []byte(s)))
		}
	}
	podInfo.InitContainers = initContainers

	containers := k8sconv.PodContainers(ctx, pod, pod.Status.ContainerStatuses)
	if !isNew {
		names := restartedContainerNames(podInfo.Containers, containers)
		for _, name := range names {
			s := fmt.Sprintf("Detected container restart. Pod: %s. Container: %s.", podInfo.PodID, name)
			handleLogAction(state, store.NewLogAction(manifest.Name, podInfo.SpanID, logger.WarnLvl, nil, []byte(s)))
		}
	}
	podInfo.Containers = containers

	if isNew {
		// This is the first time we've seen this pod.
		// Ignore any restarts that happened before Tilt saw it.
		//
		// This can happen when the image was deployed on a previous
		// Tilt run, so we're just attaching to an existing pod
		// with some old history.
		podInfo.BaselineRestarts = podInfo.AllContainerRestarts()
	}

	if len(podInfo.Containers) == 0 {
		// not enough info to do anything else
		return
	}

	if podInfo.AllContainersReady() || podInfo.Phase == v1.PodSucceeded {
		runtime := ms.K8sRuntimeState()
		runtime.LastReadyOrSucceededTime = time.Now()
		ms.RuntimeState = runtime
	}

	fwdsValid := portforward.PortForwardsAreValid(manifest, *podInfo)
	if !fwdsValid {
		logger.Get(ctx).Warnf(
			"Resource %s is using port forwards, but no container ports on pod %s",
			manifest.Name, podInfo.PodID)
	}
	checkForContainerCrash(ctx, state, mt)
}

func restartedContainerNames(existingContainers []store.Container, newContainers []store.Container) []container.Name {
	result := []container.Name{}
	for i, c := range newContainers {
		if i >= len(existingContainers) {
			break
		}

		existing := existingContainers[i]
		if existing.Name != c.Name {
			continue
		}

		if c.Restarts > existing.Restarts {
			result = append(result, c.Name)
		}
	}
	return result
}

// Find the ManifestTarget for the PodChangeAction,
// and confirm that it matches what we've deployed.
func matchPodChangeToManifest(state *store.EngineState, action k8swatch.PodChangeAction) *store.ManifestTarget {
	manifestName := action.ManifestName
	matchedAncestorUID := action.MatchedAncestorUID
	mt, ok := state.ManifestTargets[manifestName]
	if !ok {
		// This is OK. The user could have edited the manifest recently.
		return nil
	}

	ms := mt.State
	runtime := ms.K8sRuntimeState()

	// If the event has an ancestor UID attached, but that ancestor isn't in the
	// deployed UID set anymore, we can ignore it.
	isAncestorMatched := matchedAncestorUID != ""
	if isAncestorMatched && !runtime.DeployedUIDSet.Contains(matchedAncestorUID) {
		return nil
	}
	return mt
}

// Checks the runtime state if we're already tracking this pod.
// If not, AND if the pod matches the current deploy, create a new tracking object.
// Returns a store.Pod that the caller can mutate, and true
// if this is the first time we've seen this pod.
func maybeTrackPod(ms *store.ManifestState, action k8swatch.PodChangeAction) (*store.Pod, bool) {
	pod := action.Pod
	podID := k8s.PodIDFromPod(pod)
	runtime := ms.K8sRuntimeState()
	isCurrentDeploy := runtime.HasOKPodTemplateSpecHash(pod) // is pod from the most recent Tilt deploy?

	// Only attach a new pod to the runtime state if it's from the current deploy;
	// if it's from an old deploy/an old Tilt run, we don't want to be checking it
	// for status etc.
	if !isCurrentDeploy {
		return runtime.Pods[podID], false
	}

	// Case 1: We haven't seen pods for this ancestor yet.
	matchedAncestorUID := action.MatchedAncestorUID
	isAncestorMatch := matchedAncestorUID != ""
	if runtime.PodAncestorUID == "" ||
		(isAncestorMatch && runtime.PodAncestorUID != matchedAncestorUID) {

		// Track a new ancestor ID, and delete all existing tracked pods.
		runtime.Pods = make(map[k8s.PodID]*store.Pod)
		runtime.PodAncestorUID = matchedAncestorUID
		ms.RuntimeState = runtime

		// Fall through to the case below to create a new tracked pod.
	}

	podInfo, ok := runtime.Pods[podID]
	if !ok {
		// CASE 2: We have a set of pods for this ancestor UID, but not this
		// particular pod -- record it
		podInfo = &store.Pod{
			PodID: podID,
		}

		runtime.Pods[podID] = podInfo

		return podInfo, true
	}

	// CASE 3: This pod is already in the PodSet, nothing to do.
	return podInfo, false
}

func checkForContainerCrash(ctx context.Context, state *store.EngineState, mt *store.ManifestTarget) {
	ms := mt.State
	if ms.NeedsRebuildFromCrash {
		// We're already aware the pod is crashing.
		return
	}

	runningContainers := store.AllRunningContainers(mt)
	hitList := make(map[container.ID]bool, len(ms.LiveUpdatedContainerIDs))
	for cID := range ms.LiveUpdatedContainerIDs {
		hitList[cID] = true
	}
	for _, c := range runningContainers {
		delete(hitList, c.ContainerID)
	}

	if len(hitList) == 0 {
		// The pod is what we expect it to be.
		return
	}

	// The pod isn't what we expect!
	ms.NeedsRebuildFromCrash = true
	ms.LiveUpdatedContainerIDs = container.NewIDSet()
	msg := fmt.Sprintf("Detected a container change for %s. We could be running stale code. Rebuilding and deploying a new image.", ms.Name)
	le := store.NewLogAction(ms.Name, ms.LastBuild().SpanID, logger.WarnLvl, nil, []byte(msg+"\n"))
	handleLogAction(state, le)
}

// If there's more than one pod, prune the deleting/dead ones so
// that they don't clutter the output.
func prunePods(ms *store.ManifestState) {
	// Always remove pods that were manually deleted.
	runtime := ms.K8sRuntimeState()
	for key, pod := range runtime.Pods {
		if pod.Deleting {
			delete(runtime.Pods, key)
		}
	}
	// Continue pruning until we have 1 pod.
	for runtime.PodLen() > 1 {
		bestPod := ms.MostRecentPod()

		for key, pod := range runtime.Pods {
			// Remove terminated pods if they aren't the most recent one.
			isDead := pod.Phase == v1.PodSucceeded || pod.Phase == v1.PodFailed
			if isDead && pod.PodID != bestPod.PodID {
				delete(runtime.Pods, key)
				break
			}
		}

		// found nothing to delete, break out
		// NOTE(dmiller): above comment is probably erroneous, but disabling this check because I'm not sure if this is safe to change
		// original static analysis error:
		// SA4004: the surrounding loop is unconditionally terminated (staticcheck)
		//nolint:staticcheck
		return
	}
}

func handlePodResetRestartsAction(state *store.EngineState, action store.PodResetRestartsAction) {
	ms, ok := state.ManifestState(action.ManifestName)
	if !ok {
		return
	}

	runtime := ms.K8sRuntimeState()
	podInfo, ok := runtime.Pods[action.PodID]
	if !ok {
		return
	}

	// We have to be careful here because the pod might have restarted
	// since the action was created.
	delta := podInfo.VisibleContainerRestarts() - action.VisibleRestarts
	podInfo.BaselineRestarts = podInfo.AllContainerRestarts() - delta
}
