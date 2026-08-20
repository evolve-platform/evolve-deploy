package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/evolve-platform/evolve-deploy/internal/image"
	"github.com/evolve-platform/evolve-deploy/internal/logging"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// ecsStableTimeout bounds the wait for a rollout. ECS itself gives up long
// before this when the circuit breaker is on; the timeout is here so a stuck
// deploy fails the pipeline instead of hanging it.
const ecsStableTimeout = 15 * time.Minute

// ecsAppContainer is what the evolve-platform ecs-service module calls the
// application container; the reverse proxy beside it is "reverse-proxy". Only
// consulted when a task has several containers and none is named in the config.
const ecsAppContainer = "app"

// ecsPayload carries everything Apply needs, so it does not re-read state that
// may have moved since the plan was shown to the user.
type ecsPayload struct {
	register *ecs.RegisterTaskDefinitionInput
	previous string // task definition ARN the service runs now, for rollback
}

// planECS builds the next task definition from Terraform's base family.
//
// ECS keeps image, environment, cpu and healthcheck in one immutable
// container_definitions blob, so Terraform and this tool cannot each own part
// of it. Instead they own separate families: Terraform registers the shape into
// <base>, nothing points at it, and evolve-deploy derives the running family
// from it. A memory change in Terraform therefore lands on the next deploy
// without touching the image, and Terraform can never roll the image back.
func (d *Driver) planECS(ctx context.Context, want *target.Desired) (*target.Change, error) {
	t := want.Target

	base, err := d.describeTaskDef(ctx, t.Base)
	if err != nil {
		return nil, fmt.Errorf("base task definition %q: %w", t.Base, err)
	}

	current, currentARN, err := d.describeServiceTaskDef(ctx, t.Cluster, t.Name)
	if err != nil {
		return nil, err
	}

	name, err := target.PickContainer(
		containerNames(base.ContainerDefinitions), t.Container, ecsAppContainer)
	if err != nil {
		return nil, fmt.Errorf("base task definition %s: %w", t.Base, err)
	}
	slog.Debug("container chosen", "base", t.Base, "revision", base.Revision,
		"container", name, "of", strings.Join(containerNames(base.ContainerDefinitions), ","))
	container := findContainer(base.ContainerDefinitions, name)

	img, err := image.Retag(awssdk.ToString(container.Image), want.Version)
	if err != nil {
		return nil, err
	}
	if err := d.verifyImage(ctx, img); err != nil {
		return nil, err
	}

	desired := renderTaskDef(base, t.Name, name, img, want.Env, want.ManageEnv)

	from := currentImageTag(current, name)
	ch := &target.Change{
		Service:     want.Service,
		Target:      t,
		FromVersion: from,
		ToVersion:   want.Version,
		Payload:     &ecsPayload{register: desired, previous: currentARN},
	}

	wantFP := fingerprintInput(desired)
	haveFP := fingerprintTaskDef(current)
	if wantFP.equal(haveFP) {
		return nil, nil
	}

	ch.EnvAdded, ch.EnvChanged, ch.EnvRemoved = diffEnv(
		haveFP.container(name), wantFP.container(name))
	ch.Reason = ecsReason(base, current, from, want.Version, wantFP, haveFP, name)
	return ch, nil
}

func (d *Driver) applyECS(ctx context.Context, ch *target.Change) error {
	p := ch.Payload.(*ecsPayload)

	target.Status(ctx, "the task definition to be registered")
	timer := logging.Start("register task definition", "family", ch.Target.Name)
	registered, err := d.ecs.RegisterTaskDefinition(ctx, p.register)
	if err != nil {
		return fmt.Errorf("register task definition: %w", err)
	}
	arn := awssdk.ToString(registered.TaskDefinition.TaskDefinitionArn)
	timer.Done("arn", arn)

	if _, err := d.ecs.UpdateService(ctx, &ecs.UpdateServiceInput{
		Cluster:        awssdk.String(ch.Target.Cluster),
		Service:        awssdk.String(ch.Target.Name),
		TaskDefinition: awssdk.String(arn),
	}); err != nil {
		return fmt.Errorf("update service: %w", err)
	}

	target.Status(ctx, "the service to become stable")
	wait := logging.Start("waiting for the service to become stable",
		"cluster", ch.Target.Cluster, "service", ch.Target.Name, "timeout", ecsStableTimeout)
	waiter := ecs.NewServicesStableWaiter(d.ecs)
	err = waiter.Wait(ctx, &ecs.DescribeServicesInput{
		Cluster:  awssdk.String(ch.Target.Cluster),
		Services: []string{ch.Target.Name},
	}, ecsStableTimeout)
	if err == nil {
		wait.Done()
		return nil
	}

	// With deploymentCircuitBreaker { rollback = true } — which the platform
	// module should set — ECS has already put the previous task definition back
	// by the time the waiter gives up. Pointing the service at it again is
	// still correct and is what makes this work when the circuit breaker is
	// off, so do it either way and report the original failure.
	if p.previous != "" {
		if _, rbErr := d.ecs.UpdateService(ctx, &ecs.UpdateServiceInput{
			Cluster:        awssdk.String(ch.Target.Cluster),
			Service:        awssdk.String(ch.Target.Name),
			TaskDefinition: awssdk.String(p.previous),
		}); rbErr != nil {
			return errors.Join(
				fmt.Errorf("rollout failed and rolling back to %s also failed", p.previous),
				err, rbErr)
		}
	}
	return fmt.Errorf("rollout failed, rolled back to %s: %w", p.previous, err)
}

// revertECS points the service back at the task definition it ran before this
// change, used when a sibling target of the same service failed.
func (d *Driver) revertECS(ctx context.Context, ch *target.Change) error {
	p := ch.Payload.(*ecsPayload)
	if p.previous == "" {
		return fmt.Errorf("no previous task definition recorded for %s", ch.Target.Name)
	}
	if _, err := d.ecs.UpdateService(ctx, &ecs.UpdateServiceInput{
		Cluster:        awssdk.String(ch.Target.Cluster),
		Service:        awssdk.String(ch.Target.Name),
		TaskDefinition: awssdk.String(p.previous),
	}); err != nil {
		return err
	}
	waiter := ecs.NewServicesStableWaiter(d.ecs)
	return waiter.Wait(ctx, &ecs.DescribeServicesInput{
		Cluster:  awssdk.String(ch.Target.Cluster),
		Services: []string{ch.Target.Name},
	}, ecsStableTimeout)
}

// renderTaskDef copies Terraform's base definition and replaces only what this
// tool owns: the image tag on one container, and — when the config says
// anything about environment variables — that container's environment.
//
// When it does not, the environment from the base definition is carried over
// untouched. Writing an empty list instead would delete every variable
// Terraform set, which is the opposite of what a config that mentions no
// variables is asking for.
func renderTaskDef(
	base *ecstypes.TaskDefinition,
	family, containerName, image string,
	env []target.EnvVar,
	manageEnv bool,
) *ecs.RegisterTaskDefinitionInput {
	containers := make([]ecstypes.ContainerDefinition, len(base.ContainerDefinitions))
	copy(containers, base.ContainerDefinitions)

	for i := range containers {
		if awssdk.ToString(containers[i].Name) != containerName {
			continue
		}
		containers[i].Image = awssdk.String(image)
		if manageEnv {
			containers[i].Environment, containers[i].Secrets = splitEnv(env)
		}
	}

	// The family is the target name, not the base name: the base family stays
	// Terraform's and is never pointed at by a service.
	return &ecs.RegisterTaskDefinitionInput{
		Family:                  awssdk.String(family),
		ContainerDefinitions:    containers,
		Cpu:                     base.Cpu,
		Memory:                  base.Memory,
		NetworkMode:             base.NetworkMode,
		RequiresCompatibilities: base.RequiresCompatibilities,
		ExecutionRoleArn:        base.ExecutionRoleArn,
		TaskRoleArn:             base.TaskRoleArn,
		Volumes:                 base.Volumes,
		RuntimePlatform:         base.RuntimePlatform,
		EphemeralStorage:        base.EphemeralStorage,
		IpcMode:                 base.IpcMode,
		PidMode:                 base.PidMode,
		PlacementConstraints:    base.PlacementConstraints,
		ProxyConfiguration:      base.ProxyConfiguration,
	}
}

// splitEnv separates literals from references. ECS takes literals in
// `environment` and references in `secrets`, whose valueFrom accepts both
// Secrets Manager ARNs and SSM parameter names — which is why ECS needs no
// value to be read by this tool at all.
func splitEnv(env []target.EnvVar) ([]ecstypes.KeyValuePair, []ecstypes.Secret) {
	var literals []ecstypes.KeyValuePair
	var secrets []ecstypes.Secret
	for _, e := range env {
		if e.Value.IsRef() {
			secrets = append(secrets, ecstypes.Secret{
				Name:      awssdk.String(e.Name),
				ValueFrom: awssdk.String(e.Value.Name),
			})
			continue
		}
		literals = append(literals, ecstypes.KeyValuePair{
			Name:  awssdk.String(e.Name),
			Value: awssdk.String(e.Value.Literal),
		})
	}
	return literals, secrets
}

func (d *Driver) describeTaskDef(ctx context.Context, ref string) (*ecstypes.TaskDefinition, error) {
	// Passing a bare family name returns the latest ACTIVE revision, which is
	// exactly what Terraform last registered.
	out, err := d.ecs.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: awssdk.String(ref),
	})
	if err != nil {
		return nil, err
	}
	return out.TaskDefinition, nil
}

func (d *Driver) describeServiceTaskDef(
	ctx context.Context, cluster, service string,
) (*ecstypes.TaskDefinition, string, error) {
	out, err := d.ecs.DescribeServices(ctx, &ecs.DescribeServicesInput{
		Cluster:  awssdk.String(cluster),
		Services: []string{service},
	})
	if err != nil {
		return nil, "", fmt.Errorf("describe service %s/%s: %w", cluster, service, err)
	}
	if len(out.Services) == 0 || awssdk.ToString(out.Services[0].Status) == "INACTIVE" {
		return nil, "", fmt.Errorf("service %s does not exist in cluster %s", service, cluster)
	}

	arn := awssdk.ToString(out.Services[0].TaskDefinition)
	td, err := d.describeTaskDef(ctx, arn)
	if err != nil {
		return nil, "", fmt.Errorf("current task definition %s: %w", arn, err)
	}
	return td, arn, nil
}

func findContainer(containers []ecstypes.ContainerDefinition, name string) *ecstypes.ContainerDefinition {
	for i := range containers {
		if awssdk.ToString(containers[i].Name) == name {
			return &containers[i]
		}
	}
	return nil
}

func containerNames(containers []ecstypes.ContainerDefinition) []string {
	out := make([]string, len(containers))
	for i, c := range containers {
		out[i] = awssdk.ToString(c.Name)
	}
	return out
}

func currentImageTag(td *ecstypes.TaskDefinition, container string) string {
	c := findContainer(td.ContainerDefinitions, container)
	if c == nil {
		return ""
	}
	return image.Tag(awssdk.ToString(c.Image))
}

// --- comparison -------------------------------------------------------------
//
// The diff is structural rather than a version check, because a deploy is also
// needed when Terraform moved the base: new memory, a changed healthcheck. That
// change carries no new version, so comparing tags alone would silently never
// apply it.

type taskFP struct {
	Containers []containerFP
	Task       string
}

type containerFP struct {
	Name    string
	Image   string
	Env     map[string]string
	Secrets map[string]string
	Rest    string
}

func (f taskFP) container(name string) containerFP {
	for _, c := range f.Containers {
		if c.Name == name {
			return c
		}
	}
	return containerFP{}
}

func (f taskFP) equal(other taskFP) bool {
	a, _ := json.Marshal(f)
	b, _ := json.Marshal(other)
	return string(a) == string(b)
}

func fingerprintInput(in *ecs.RegisterTaskDefinitionInput) taskFP {
	return taskFP{
		Containers: fingerprintContainers(in.ContainerDefinitions),
		Task: taskLevelFP(in.Cpu, in.Memory, string(in.NetworkMode),
			in.ExecutionRoleArn, in.TaskRoleArn, in.RequiresCompatibilities,
			in.Volumes, in.RuntimePlatform, in.EphemeralStorage),
	}
}

func fingerprintTaskDef(td *ecstypes.TaskDefinition) taskFP {
	if td == nil {
		return taskFP{}
	}
	return taskFP{
		Containers: fingerprintContainers(td.ContainerDefinitions),
		Task: taskLevelFP(td.Cpu, td.Memory, string(td.NetworkMode),
			td.ExecutionRoleArn, td.TaskRoleArn, td.RequiresCompatibilities,
			td.Volumes, td.RuntimePlatform, td.EphemeralStorage),
	}
}

func taskLevelFP(cpu, memory *string, networkMode string, execRole, taskRole *string,
	compat []ecstypes.Compatibility, volumes []ecstypes.Volume,
	platform *ecstypes.RuntimePlatform, storage *ecstypes.EphemeralStorage,
) string {
	blob, _ := json.Marshal(struct {
		Cpu, Memory, NetworkMode, ExecRole, TaskRole string
		Compat                                       []ecstypes.Compatibility
		Volumes                                      []ecstypes.Volume
		Platform                                     *ecstypes.RuntimePlatform
		Storage                                      *ecstypes.EphemeralStorage
	}{
		Cpu:         awssdk.ToString(cpu),
		Memory:      awssdk.ToString(memory),
		NetworkMode: networkMode,
		ExecRole:    awssdk.ToString(execRole),
		TaskRole:    awssdk.ToString(taskRole),
		Compat:      compat,
		Volumes:     volumes,
		Platform:    platform,
		Storage:     storage,
	})
	return string(blob)
}

func fingerprintContainers(in []ecstypes.ContainerDefinition) []containerFP {
	out := make([]containerFP, 0, len(in))
	for _, c := range in {
		fp := containerFP{
			Name:    awssdk.ToString(c.Name),
			Image:   awssdk.ToString(c.Image),
			Env:     map[string]string{},
			Secrets: map[string]string{},
		}
		for _, kv := range c.Environment {
			fp.Env[awssdk.ToString(kv.Name)] = awssdk.ToString(kv.Value)
		}
		for _, s := range c.Secrets {
			fp.Secrets[awssdk.ToString(s.Name)] = awssdk.ToString(s.ValueFrom)
		}

		// Everything else on the container comes from the base definition, so
		// any difference here means Terraform changed the shape.
		rest := c
		rest.Name = nil
		rest.Image = nil
		rest.Environment = nil
		rest.Secrets = nil
		blob, _ := json.Marshal(rest)
		fp.Rest = string(blob)

		out = append(out, fp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func diffEnv(have, want containerFP) (added, changed, removed []string) {
	merge := func(fp containerFP) map[string]string {
		m := map[string]string{}
		for k, v := range fp.Env {
			m[k] = "=" + v
		}
		// Compare references by what they point at, not by their value: a
		// reference that still names the same secret has not changed, whatever
		// the secret now holds.
		for k, v := range fp.Secrets {
			m[k] = "->" + v
		}
		return m
	}
	h, w := merge(have), merge(want)

	for k, wv := range w {
		hv, ok := h[k]
		switch {
		case !ok:
			added = append(added, k)
		case hv != wv:
			changed = append(changed, k)
		}
	}
	for k := range h {
		if _, ok := w[k]; !ok {
			removed = append(removed, k)
		}
	}
	sort.Strings(added)
	sort.Strings(changed)
	sort.Strings(removed)
	return added, changed, removed
}

// ecsReason says why a deploy is happening. Without it, a rollout triggered by
// a Terraform-side memory change looks like the tool acting at random: nothing
// in the deploy config moved.
func ecsReason(
	base, current *ecstypes.TaskDefinition,
	from, to string,
	want, have taskFP,
	container string,
) string {
	var parts []string
	if from != to {
		parts = append(parts, fmt.Sprintf("version %s -> %s", orNone(from), to))
	}

	w, h := want.container(container), have.container(container)
	if w.Rest != h.Rest || want.Task != have.Task {
		parts = append(parts, fmt.Sprintf("base %s:%d changed",
			awssdk.ToString(base.Family), base.Revision))
	}
	if current == nil {
		parts = append(parts, "service has no task definition yet")
	}
	if len(parts) == 0 {
		return "environment changed"
	}
	return strings.Join(parts, ", ")
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
