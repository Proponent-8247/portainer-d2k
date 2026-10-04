package adapter

import (
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// ExecOptions mirrors the subset of docker exec flags d2k supports.
type ExecOptions struct {
	Name         string
	Cmd          []string
	AttachStdin  bool
	AttachStdout bool
	AttachStderr bool
	Tty          bool
}

func (a *KubernetesDockerAdapter) execInPod(
	ctx context.Context,
	podName string,
	containerName string,
	command []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	tty bool,
) error {
	req := a.client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(a.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   command,
			Stdin:     stdin != nil,
			Stdout:    stdout != nil,
			Stderr:    stderr != nil,
			TTY:       tty,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(a.restConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("unable to create SPDY executor: %w", err)
	}

	return executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
		Tty:    tty,
	})
}

// ExecContainer executes a command in the container's pod.
func (a *KubernetesDockerAdapter) ExecContainer(ctx context.Context, opts ExecOptions, stdin io.Reader, stdout, stderr io.Writer) error {
	resolved, err := a.resolveDeploymentName(ctx, opts.Name)
	if err != nil {
		return err
	}

	pod, err := a.currentPodForDeployment(ctx, resolved)
	if err != nil {
		return err
	}

	containerName := resolved
	if len(pod.Spec.Containers) > 0 {
		containerName = pod.Spec.Containers[0].Name
	}

	return a.execInPod(ctx, pod.Name, containerName, opts.Cmd, stdin, stdout, stderr, opts.Tty)
}
