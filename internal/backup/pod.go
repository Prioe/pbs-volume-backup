package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

const (
	dataPath   = "/data"
	secretPath = "/secret"
	tokenFile  = "password"
	keyPath    = "/key"
	keyFile    = "key"
	clientPath = "/usr/bin/proxmox-backup-client"
)

var errNotStarted = errors.New("not started")

// clientPod describes a pod that runs proxmox-backup-client once.
type clientPod struct {
	meta     metav1.ObjectMeta
	args     []string
	caps     []corev1.Capability
	deadline time.Duration
	volumes  []corev1.Volume
	mounts   []corev1.VolumeMount
	// withKey passes the encryption key, if one is configured. snapshot
	// list rejects --keyfile.
	withKey bool
}

// secrets names the Secrets in the pod's namespace that hold the token and
// the encryption key. They can be the same Secret.
type secrets struct {
	token string
	key   string
}

// build returns the pod. It runs as root with only caps, mounts the token
// at secretPath and the key at keyPath, and keeps the root filesystem
// read-only.
func (p clientPod) build(c *Client, s secrets) *corev1.Pod {
	volumes := slices.Concat(p.volumes, []corev1.Volume{
		{Name: "secret", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: s.token,
				Items:      []corev1.KeyToPath{{Key: c.TokenSecretKey, Path: tokenFile}},
			},
		}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "home", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	})
	mounts := slices.Concat(p.mounts, []corev1.VolumeMount{
		{Name: "secret", MountPath: secretPath, ReadOnly: true},
		{Name: "tmp", MountPath: "/tmp"},
		{Name: "home", MountPath: "/root"},
	})
	args := slices.Clone(p.args)
	if c.PBSNamespace != "" {
		args = append(args, "--ns", c.PBSNamespace)
	}
	if p.withKey && c.KeySecret != "" {
		volumes = append(volumes, corev1.Volume{Name: "key", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: s.key,
				Items:      []corev1.KeyToPath{{Key: c.KeySecretKey, Path: keyFile}},
			},
		}})
		mounts = append(mounts, corev1.VolumeMount{Name: "key", MountPath: keyPath, ReadOnly: true})
		args = append(args, "--keyfile", keyPath+"/"+keyFile)
	}
	return &corev1.Pod{
		ObjectMeta: p.meta,
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:         ptr.To(int64(p.deadline.Seconds())),
			AutomountServiceAccountToken:  ptr.To(false),
			EnableServiceLinks:            ptr.To(false),
			TerminationGracePeriodSeconds: ptr.To(int64(10)),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser:      ptr.To(int64(0)),
				RunAsGroup:     ptr.To(int64(0)),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:    "client",
				Image:   c.Image,
				Command: []string{clientPath},
				Args:    args,
				Env: []corev1.EnvVar{
					{Name: "PBS_REPOSITORY", Value: c.Repository},
					{Name: "PBS_FINGERPRINT", Value: c.Fingerprint},
					{Name: "PBS_PASSWORD_FILE", Value: secretPath + "/" + tokenFile},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false),
					ReadOnlyRootFilesystem:   ptr.To(true),
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"},
						Add:  p.caps,
					},
				},
				VolumeMounts: mounts,
			}},
			Volumes: volumes,
		},
	}
}

// logReader returns the log of a pod.
type logReader func(ctx context.Context, namespace, name string) string

func kubeLogs(kube kubernetes.Interface) logReader {
	return func(ctx context.Context, namespace, name string) string {
		return podLogs(ctx, kube, namespace, name)
	}
}

// waitPod waits until the pod has finished and returns its log. It fails if
// the pod stays Pending longer than startTimeout, or if the pod failed.
func waitPod(ctx context.Context, kube kubernetes.Interface, logs logReader, namespace, name, what string, startTimeout, runTimeout, poll time.Duration) (string, error) {
	pods := kube.CoreV1().Pods(namespace)
	var pod *corev1.Pod
	start := time.Now()
	err := wait.PollUntilContextTimeout(ctx, poll, startTimeout+runTimeout+time.Minute, true, func(ctx context.Context) (bool, error) {
		var err error
		pod, err = pods.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if pod.Status.Phase == corev1.PodPending && time.Since(start) > startTimeout {
			return false, fmt.Errorf("%s %w within %s", what, errNotStarted, startTimeout)
		}
		return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed, nil
	})
	if err != nil {
		if pod != nil {
			return "", fmt.Errorf("%s did not finish: %w (%s)", what, err, podState(pod))
		}
		return "", fmt.Errorf("%s did not finish: %w", what, err)
	}
	out := logs(ctx, namespace, name)
	if pod.Status.Phase == corev1.PodFailed {
		return out, fmt.Errorf("%s failed (%s): %s", what, podState(pod), lastLines(out, 10))
	}
	return out, nil
}

func podLogs(ctx context.Context, kube kubernetes.Interface, namespace, name string) string {
	rc, err := kube.CoreV1().Pods(namespace).GetLogs(name, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		return "(logs unavailable: " + err.Error() + ")"
	}
	defer func() { _ = rc.Close() }()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(rc, 1<<20)); err != nil {
		return "(logs unavailable: " + err.Error() + ")"
	}
	return buf.String()
}

func podState(pod *corev1.Pod) string {
	parts := []string{"phase " + string(pod.Status.Phase)}
	if pod.Status.Reason != "" {
		parts = append(parts, "reason "+pod.Status.Reason)
	}
	for _, c := range pod.Status.Conditions {
		if c.Status == corev1.ConditionFalse && c.Reason != "" {
			parts = append(parts, fmt.Sprintf("%s: %s %s", c.Type, c.Reason, c.Message))
		}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		switch {
		case cs.State.Terminated != nil:
			parts = append(parts, fmt.Sprintf("exit code %d", cs.State.Terminated.ExitCode))
		case cs.State.Waiting != nil:
			parts = append(parts, strings.TrimSpace("waiting: "+cs.State.Waiting.Reason+" "+cs.State.Waiting.Message))
		}
	}
	return strings.Join(parts, ", ")
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func deletePod(ctx context.Context, kube kubernetes.Interface, namespace, name string) error {
	return ignoreNotFound(kube.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: ptr.To(int64(0))}))
}
