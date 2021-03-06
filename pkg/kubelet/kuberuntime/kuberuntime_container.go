/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kuberuntime

import (
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"os"
	"path"
	"sort"
	"sync"
	"time"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/unversioned"
	runtimeApi "k8s.io/kubernetes/pkg/kubelet/api/v1alpha1/runtime"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/dockershim"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	"k8s.io/kubernetes/pkg/types"
	utilruntime "k8s.io/kubernetes/pkg/util/runtime"
	"k8s.io/kubernetes/pkg/util/term"
)

// startContainer starts a container and returns a message indicates why it is failed on error.
// It starts the container through the following steps:
// * pull the image
// * create the container
// * start the container
// * run the post start lifecycle hooks (if applicable)
func (m *kubeGenericRuntimeManager) startContainer(podSandboxID string, podSandboxConfig *runtimeApi.PodSandboxConfig, container *api.Container, pod *api.Pod, podStatus *kubecontainer.PodStatus, pullSecrets []api.Secret, podIP string) (string, error) {
	// Step 1: pull the image.
	err, msg := m.imagePuller.EnsureImageExists(pod, container, pullSecrets)
	if err != nil {
		return msg, err
	}

	// Step 2: create the container.
	ref, err := kubecontainer.GenerateContainerRef(pod, container)
	if err != nil {
		glog.Errorf("Can't make a ref to pod %q, container %v: %v", format.Pod(pod), container.Name, err)
	}
	glog.V(4).Infof("Generating ref for container %s: %#v", container.Name, ref)

	// For a new container, the RestartCount should be 0
	restartCount := 0
	containerStatus := podStatus.FindContainerStatusByName(container.Name)
	if containerStatus != nil {
		restartCount = containerStatus.RestartCount + 1
	}

	containerConfig, err := m.generateContainerConfig(container, pod, restartCount, podIP)
	if err != nil {
		m.recorder.Eventf(ref, api.EventTypeWarning, events.FailedToCreateContainer, "Failed to create container with error: %v", err)
		return "Generate Container Config Failed", err
	}
	containerID, err := m.runtimeService.CreateContainer(podSandboxID, containerConfig, podSandboxConfig)
	if err != nil {
		m.recorder.Eventf(ref, api.EventTypeWarning, events.FailedToCreateContainer, "Failed to create container with error: %v", err)
		return "Create Container Failed", err
	}
	m.recorder.Eventf(ref, api.EventTypeNormal, events.CreatedContainer, "Created container with id %v", containerID)
	if ref != nil {
		m.containerRefManager.SetRef(kubecontainer.ContainerID{
			Type: m.runtimeName,
			ID:   containerID,
		}, ref)
	}

	// Step 3: start the container.
	err = m.runtimeService.StartContainer(containerID)
	if err != nil {
		m.recorder.Eventf(ref, api.EventTypeWarning, events.FailedToStartContainer,
			"Failed to start container with id %v with error: %v", containerID, err)
		return "Start Container Failed", err
	}
	m.recorder.Eventf(ref, api.EventTypeNormal, events.StartedContainer, "Started container with id %v", containerID)

	// Step 4: execute the post start hook.
	if container.Lifecycle != nil && container.Lifecycle.PostStart != nil {
		kubeContainerID := kubecontainer.ContainerID{
			Type: m.runtimeName,
			ID:   containerID,
		}
		msg, handlerErr := m.runner.Run(kubeContainerID, pod, container, container.Lifecycle.PostStart)
		if handlerErr != nil {
			err := fmt.Errorf("PostStart handler: %v", handlerErr)
			m.generateContainerEvent(kubeContainerID, api.EventTypeWarning, events.FailedPostStartHook, msg)
			m.killContainer(pod, kubeContainerID, container, "FailedPostStartHook", nil)
			return "PostStart Hook Failed", err
		}
	}

	return "", nil
}

// getContainerLogsPath gets log path for container.
func getContainerLogsPath(containerName string, podUID types.UID) string {
	return path.Join(podLogsRootDirectory, string(podUID), fmt.Sprintf("%s.log", containerName))
}

// generateContainerConfig generates container config for kubelet runtime api.
func (m *kubeGenericRuntimeManager) generateContainerConfig(container *api.Container, pod *api.Pod, restartCount int, podIP string) (*runtimeApi.ContainerConfig, error) {
	opts, err := m.runtimeHelper.GenerateRunContainerOptions(pod, container, podIP)
	if err != nil {
		return nil, err
	}

	command, args := kubecontainer.ExpandContainerCommandAndArgs(container, opts.Envs)
	containerLogsPath := getContainerLogsPath(container.Name, pod.UID)
	podHasSELinuxLabel := pod.Spec.SecurityContext != nil && pod.Spec.SecurityContext.SELinuxOptions != nil
	restartCountUint32 := uint32(restartCount)
	config := &runtimeApi.ContainerConfig{
		Metadata: &runtimeApi.ContainerMetadata{
			Name:    &container.Name,
			Attempt: &restartCountUint32,
		},
		Image:       &runtimeApi.ImageSpec{Image: &container.Image},
		Command:     command,
		Args:        args,
		WorkingDir:  &container.WorkingDir,
		Labels:      newContainerLabels(container, pod),
		Annotations: newContainerAnnotations(container, pod, restartCount),
		Mounts:      makeMounts(opts, container, podHasSELinuxLabel),
		LogPath:     &containerLogsPath,
		Stdin:       &container.Stdin,
		StdinOnce:   &container.StdinOnce,
		Tty:         &container.TTY,
		Linux:       m.generateLinuxContainerConfig(container),
	}

	// set privileged and readonlyRootfs
	if container.SecurityContext != nil {
		securityContext := container.SecurityContext
		if securityContext.Privileged != nil {
			config.Privileged = securityContext.Privileged
		}
		if securityContext.ReadOnlyRootFilesystem != nil {
			config.ReadonlyRootfs = securityContext.ReadOnlyRootFilesystem
		}
	}

	// set environment variables
	envs := make([]*runtimeApi.KeyValue, len(opts.Envs))
	for idx := range opts.Envs {
		e := opts.Envs[idx]
		envs[idx] = &runtimeApi.KeyValue{
			Key:   &e.Name,
			Value: &e.Value,
		}
	}
	config.Envs = envs

	return config, nil
}

// generateLinuxContainerConfig generates linux container config for kubelet runtime api.
func (m *kubeGenericRuntimeManager) generateLinuxContainerConfig(container *api.Container) *runtimeApi.LinuxContainerConfig {
	linuxConfig := &runtimeApi.LinuxContainerConfig{
		Resources: &runtimeApi.LinuxContainerResources{},
	}

	// set linux container resources
	var cpuShares int64
	cpuRequest := container.Resources.Requests.Cpu()
	cpuLimit := container.Resources.Limits.Cpu()
	memoryLimit := container.Resources.Limits.Memory().Value()
	// If request is not specified, but limit is, we want request to default to limit.
	// API server does this for new containers, but we repeat this logic in Kubelet
	// for containers running on existing Kubernetes clusters.
	if cpuRequest.IsZero() && !cpuLimit.IsZero() {
		cpuShares = milliCPUToShares(cpuLimit.MilliValue())
	} else {
		// if cpuRequest.Amount is nil, then milliCPUToShares will return the minimal number
		// of CPU shares.
		cpuShares = milliCPUToShares(cpuRequest.MilliValue())
	}
	linuxConfig.Resources.CpuShares = &cpuShares
	if memoryLimit != 0 {
		linuxConfig.Resources.MemoryLimitInBytes = &memoryLimit
	}
	if m.cpuCFSQuota {
		// if cpuLimit.Amount is nil, then the appropriate default value is returned
		// to allow full usage of cpu resource.
		cpuQuota, cpuPeriod := milliCPUToQuota(cpuLimit.MilliValue())
		linuxConfig.Resources.CpuQuota = &cpuQuota
		linuxConfig.Resources.CpuPeriod = &cpuPeriod
	}

	// set security context options
	if container.SecurityContext != nil {
		securityContext := container.SecurityContext
		if securityContext.Capabilities != nil {
			linuxConfig.Capabilities = &runtimeApi.Capability{
				AddCapabilities:  make([]string, 0, len(securityContext.Capabilities.Add)),
				DropCapabilities: make([]string, 0, len(securityContext.Capabilities.Drop)),
			}
			for index, value := range securityContext.Capabilities.Add {
				linuxConfig.Capabilities.AddCapabilities[index] = string(value)
			}
			for index, value := range securityContext.Capabilities.Drop {
				linuxConfig.Capabilities.DropCapabilities[index] = string(value)
			}
		}

		if securityContext.SELinuxOptions != nil {
			linuxConfig.SelinuxOptions = &runtimeApi.SELinuxOption{
				User:  &securityContext.SELinuxOptions.User,
				Role:  &securityContext.SELinuxOptions.Role,
				Type:  &securityContext.SELinuxOptions.Type,
				Level: &securityContext.SELinuxOptions.Level,
			}
		}
	}

	return linuxConfig
}

// makeMounts generates container volume mounts for kubelet runtime api.
func makeMounts(opts *kubecontainer.RunContainerOptions, container *api.Container, podHasSELinuxLabel bool) []*runtimeApi.Mount {
	volumeMounts := []*runtimeApi.Mount{}

	for idx := range opts.Mounts {
		v := opts.Mounts[idx]
		m := &runtimeApi.Mount{
			Name:          &v.Name,
			HostPath:      &v.HostPath,
			ContainerPath: &v.ContainerPath,
			Readonly:      &v.ReadOnly,
		}
		if podHasSELinuxLabel && v.SELinuxRelabel {
			m.SelinuxRelabel = &v.SELinuxRelabel
		}

		volumeMounts = append(volumeMounts, m)
	}

	// The reason we create and mount the log file in here (not in kubelet) is because
	// the file's location depends on the ID of the container, and we need to create and
	// mount the file before actually starting the container.
	if opts.PodContainerDir != "" && len(container.TerminationMessagePath) != 0 {
		// Because the PodContainerDir contains pod uid and container name which is unique enough,
		// here we just add a random id to make the path unique for different instances
		// of the same container.
		cid := makeUID()
		containerLogPath := path.Join(opts.PodContainerDir, cid)
		fs, err := os.Create(containerLogPath)
		if err != nil {
			glog.Errorf("Error on creating termination-log file %q: %v", containerLogPath, err)
		} else {
			fs.Close()
			volumeMounts = append(volumeMounts, &runtimeApi.Mount{
				HostPath:      &containerLogPath,
				ContainerPath: &container.TerminationMessagePath,
			})
		}
	}

	return volumeMounts
}

// getKubeletContainers lists containers managed by kubelet.
// The boolean parameter specifies whether returns all containers including
// those already exited and dead containers (used for garbage collection).
func (m *kubeGenericRuntimeManager) getKubeletContainers(allContainers bool) ([]*runtimeApi.Container, error) {
	filter := &runtimeApi.ContainerFilter{
		LabelSelector: map[string]string{kubernetesManagedLabel: "true"},
	}
	if !allContainers {
		runningState := runtimeApi.ContainerState_RUNNING
		filter.State = &runningState
	}

	containers, err := m.getContainersHelper(filter)
	if err != nil {
		glog.Errorf("getKubeletContainers failed: %v", err)
		return nil, err
	}

	return containers, nil
}

// getContainers lists containers by filter.
func (m *kubeGenericRuntimeManager) getContainersHelper(filter *runtimeApi.ContainerFilter) ([]*runtimeApi.Container, error) {
	resp, err := m.runtimeService.ListContainers(filter)
	if err != nil {
		return nil, err
	}

	return resp, err
}

// makeUID returns a randomly generated string.
func makeUID() string {
	return fmt.Sprintf("%08x", rand.Uint32())
}

// getTerminationMessage gets termination message of the container.
func getTerminationMessage(status *runtimeApi.ContainerStatus, kubeStatus *kubecontainer.ContainerStatus, terminationMessagePath string) string {
	message := ""

	if !kubeStatus.FinishedAt.IsZero() || kubeStatus.ExitCode != 0 {
		if terminationMessagePath == "" {
			return ""
		}

		for _, mount := range status.Mounts {
			if mount.GetContainerPath() == terminationMessagePath {
				path := mount.GetHostPath()
				if data, err := ioutil.ReadFile(path); err != nil {
					message = fmt.Sprintf("Error on reading termination-log %s: %v", path, err)
				} else {
					message = string(data)
				}
				break
			}
		}
	}

	return message
}

// getKubeletContainerStatuses gets all containers' status for the pod sandbox.
func (m *kubeGenericRuntimeManager) getKubeletContainerStatuses(podSandboxID string) ([]*kubecontainer.ContainerStatus, error) {
	containers, err := m.runtimeService.ListContainers(&runtimeApi.ContainerFilter{
		PodSandboxId: &podSandboxID,
	})
	if err != nil {
		glog.Errorf("ListContainers error: %v", err)
		return nil, err
	}

	statuses := make([]*kubecontainer.ContainerStatus, len(containers))
	// TODO: optimization: set maximum number of containers per container name to examine.
	for i, c := range containers {
		status, err := m.runtimeService.ContainerStatus(c.GetId())
		if err != nil {
			glog.Errorf("ContainerStatus for %s error: %v", c.GetId(), err)
			return nil, err
		}

		annotatedInfo := getContainerInfoFromAnnotations(c.Annotations)
		labeledInfo := getContainerInfoFromLabels(c.Labels)
		cStatus := &kubecontainer.ContainerStatus{
			ID: kubecontainer.ContainerID{
				Type: m.runtimeName,
				ID:   c.GetId(),
			},
			Name:         labeledInfo.ContainerName,
			Image:        status.Image.GetImage(),
			ImageID:      status.GetImageRef(),
			Hash:         annotatedInfo.Hash,
			RestartCount: annotatedInfo.RestartCount,
			State:        toKubeContainerState(c.GetState()),
			CreatedAt:    time.Unix(status.GetCreatedAt(), 0),
		}

		if c.GetState() == runtimeApi.ContainerState_RUNNING {
			cStatus.StartedAt = time.Unix(status.GetStartedAt(), 0)
		} else {
			cStatus.Reason = status.GetReason()
			cStatus.ExitCode = int(status.GetExitCode())
			cStatus.FinishedAt = time.Unix(status.GetFinishedAt(), 0)
		}

		cStatus.Message = getTerminationMessage(status, cStatus, annotatedInfo.TerminationMessagePath)
		statuses[i] = cStatus
	}

	sort.Sort(containerStatusByCreated(statuses))
	return statuses, nil
}

// generateContainerEvent generates an event for the container.
func (m *kubeGenericRuntimeManager) generateContainerEvent(containerID kubecontainer.ContainerID, eventType, reason, message string) {
	ref, ok := m.containerRefManager.GetRef(containerID)
	if !ok {
		glog.Warningf("No ref for container %q", containerID)
		return
	}
	m.recorder.Event(ref, eventType, reason, message)
}

// executePreStopHook runs the pre-stop lifecycle hooks if applicable and returns the duration it takes.
func (m *kubeGenericRuntimeManager) executePreStopHook(pod *api.Pod, containerID kubecontainer.ContainerID, containerSpec *api.Container, gracePeriod int64) int64 {
	glog.V(3).Infof("Running preStop hook for container %q", containerID.String())

	start := unversioned.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer utilruntime.HandleCrash()
		if msg, err := m.runner.Run(containerID, pod, containerSpec, containerSpec.Lifecycle.PreStop); err != nil {
			glog.Errorf("preStop hook for container %q failed: %v", containerSpec.Name, err)
			m.generateContainerEvent(containerID, api.EventTypeWarning, events.FailedPreStopHook, msg)
		}
	}()

	select {
	case <-time.After(time.Duration(gracePeriod) * time.Second):
		glog.V(2).Infof("preStop hook for container %q did not complete in %d seconds", containerID, gracePeriod)
	case <-done:
		glog.V(3).Infof("preStop hook for container %q completed", containerID)
	}

	return int64(unversioned.Now().Sub(start.Time).Seconds())
}

// killContainer kills a container through the following steps:
// * Run the pre-stop lifecycle hooks (if applicable).
// * Stop the container.
func (m *kubeGenericRuntimeManager) killContainer(pod *api.Pod, containerID kubecontainer.ContainerID, containerSpec *api.Container, reason string, gracePeriodOverride *int64) error {
	gracePeriod := int64(minimumGracePeriodInSeconds)
	if pod != nil {
		switch {
		case pod.DeletionGracePeriodSeconds != nil:
			gracePeriod = *pod.DeletionGracePeriodSeconds
		case pod.Spec.TerminationGracePeriodSeconds != nil:
			gracePeriod = *pod.Spec.TerminationGracePeriodSeconds
		}
	}

	glog.V(2).Infof("Killing container %q with %d second grace period", containerID.String(), gracePeriod)

	// Run the pre-stop lifecycle hooks if applicable.
	if pod != nil && containerSpec != nil && containerSpec.Lifecycle != nil && containerSpec.Lifecycle.PreStop != nil {
		gracePeriod = gracePeriod - m.executePreStopHook(pod, containerID, containerSpec, gracePeriod)
	}
	// always give containers a minimal shutdown window to avoid unnecessary SIGKILLs
	if gracePeriod < minimumGracePeriodInSeconds {
		gracePeriod = minimumGracePeriodInSeconds
	}
	if gracePeriodOverride != nil {
		gracePeriod = *gracePeriodOverride
		glog.V(3).Infof("Killing container %q, but using %d second grace period override", containerID, gracePeriod)
	}

	err := m.runtimeService.StopContainer(containerID.ID, gracePeriod)
	if err != nil {
		glog.Errorf("Container %q termination failed with gracePeriod %d: %v", containerID.String(), gracePeriod, err)
	} else {
		glog.V(3).Infof("Container %q exited normally", containerID.String())
	}

	message := fmt.Sprintf("Killing container with id %s", containerID.String())
	if reason != "" {
		message = fmt.Sprint(message, ":", reason)
	}
	m.generateContainerEvent(containerID, api.EventTypeNormal, events.KillingContainer, message)
	m.containerRefManager.ClearRef(containerID)

	return err
}

// killContainersWithSyncResult kills all pod's containers with sync results.
func (m *kubeGenericRuntimeManager) killContainersWithSyncResult(pod *api.Pod, runningPod kubecontainer.Pod, gracePeriodOverride *int64) (syncResults []*kubecontainer.SyncResult) {
	containerResults := make(chan *kubecontainer.SyncResult, len(runningPod.Containers))
	wg := sync.WaitGroup{}

	wg.Add(len(runningPod.Containers))
	for _, container := range runningPod.Containers {
		go func(container *kubecontainer.Container) {
			defer utilruntime.HandleCrash()
			defer wg.Done()

			var containerSpec *api.Container
			if pod != nil {
				for i, c := range pod.Spec.Containers {
					if container.Name == c.Name {
						containerSpec = &pod.Spec.Containers[i]
						break
					}
				}
			}

			killContainerResult := kubecontainer.NewSyncResult(kubecontainer.KillContainer, container.Name)
			if err := m.killContainer(pod, container.ID, containerSpec, "Need to kill Pod", gracePeriodOverride); err != nil {
				killContainerResult.Fail(kubecontainer.ErrKillContainer, err.Error())
			}
			containerResults <- killContainerResult
		}(container)
	}
	wg.Wait()
	close(containerResults)

	for containerResult := range containerResults {
		syncResults = append(syncResults, containerResult)
	}
	return
}

// AttachContainer attaches to the container's console
func (m *kubeGenericRuntimeManager) AttachContainer(id kubecontainer.ContainerID, stdin io.Reader, stdout, stderr io.WriteCloser, tty bool, resize <-chan term.Size) (err error) {
	return fmt.Errorf("not implemented")
}

// GetContainerLogs returns logs of a specific container.
func (m *kubeGenericRuntimeManager) GetContainerLogs(pod *api.Pod, containerID kubecontainer.ContainerID, logOptions *api.PodLogOptions, stdout, stderr io.Writer) (err error) {
	// Get logs directly from docker for in-process docker integration for
	// now to unblock other tests.
	// TODO: remove this hack after setting down on how to implement log
	// retrieval/management.
	if ds, ok := m.runtimeService.(dockershim.DockerLegacyService); ok {
		return ds.GetContainerLogs(pod, containerID, logOptions, stdout, stderr)
	}
	return fmt.Errorf("not implemented")
}

// Runs the command in the container of the specified pod using nsenter.
// Attaches the processes stdin, stdout, and stderr. Optionally uses a
// tty.
// TODO: handle terminal resizing, refer https://github.com/kubernetes/kubernetes/issues/29579
func (m *kubeGenericRuntimeManager) ExecInContainer(containerID kubecontainer.ContainerID, cmd []string, stdin io.Reader, stdout, stderr io.WriteCloser, tty bool, resize <-chan term.Size) error {
	// Use `docker exec` directly for in-process docker integration for
	// now to unblock other tests.
	// TODO: remove this hack after exec is defined in CRI.
	if ds, ok := m.runtimeService.(dockershim.DockerLegacyService); ok {
		return ds.ExecInContainer(containerID, cmd, stdin, stdout, stderr, tty, resize)
	}
	return fmt.Errorf("not implemented")
}

// DeleteContainer removes a container.
func (m *kubeGenericRuntimeManager) DeleteContainer(containerID kubecontainer.ContainerID) error {
	return m.runtimeService.RemoveContainer(containerID.ID)
}
