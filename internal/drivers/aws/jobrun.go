package aws

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/hooks"
	"github.com/evolve-platform/evolve-deploy/internal/image"
	"github.com/evolve-platform/evolve-deploy/internal/logging"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// taskPoll is how often a one-off task is read while it runs. A migration takes
// seconds to minutes, and DescribeTasks is cheap.
const taskPoll = 6 * time.Second

// jobRun is a one-off ECS task, worked out from Terraform's base family and the
// service it borrows a network from.
type jobRun struct {
	register *ecs.RegisterTaskDefinitionInput
	// current is a revision of the family that is already exactly register,
	// which is the second time a release is applied. Empty means register.
	current   string
	container string
	run       *ecs.RunTaskInput
}

// CheckJob works out the run without registering or starting anything.
func (d *Driver) CheckJob(ctx context.Context, j hooks.Job) error {
	_, err := d.prepareJobRun(ctx, j)
	return err
}

// RunJob runs a one-off ECS task on the release's version and waits until it
// has stopped.
//
// ECS has no job resource, so a job here is what ecs-deplojo's before_deploy
// was: a task definition family and a RunTask. Two things are different. This
// waits, and reads the exit code — a migration that is started and not watched
// is a release that goes out on a schema it does not have. And the task gets the
// network of an ECS service: on Fargate a task with no subnets does not start.
//
// The image and the command are both in the task definition, because RunTask's
// overrides can change neither the image nor the entry point. That is also why
// nothing is put back afterwards: each run registers its own revision from
// Terraform's base, and a run without a command registers one without it.
func (d *Driver) RunJob(ctx context.Context, j hooks.Job, out io.Writer) error {
	plan, err := d.prepareJobRun(ctx, j)
	if err != nil {
		return err
	}

	arn := plan.current
	if arn == "" {
		timer := logging.Start("register task definition", "family", j.Name)
		registered, err := d.ecs.RegisterTaskDefinition(ctx, plan.register)
		if err != nil {
			return fmt.Errorf("task definition %s: register: %w", j.Name, err)
		}
		arn = awssdk.ToString(registered.TaskDefinition.TaskDefinitionArn)
		timer.Done("arn", arn)
	}

	run := *plan.run
	run.TaskDefinition = awssdk.String(arn)
	started, err := d.ecs.RunTask(ctx, &run)
	if err != nil {
		return fmt.Errorf("task %s: run: %w", j.Name, err)
	}
	if len(started.Failures) > 0 || len(started.Tasks) == 0 {
		return fmt.Errorf("task %s did not start: %s", j.Name, failures(started.Failures))
	}
	task := awssdk.ToString(started.Tasks[0].TaskArn)
	fmt.Fprintf(out, "started task %s from %s\n", taskID(task), taskID(arn))

	return d.waitTask(ctx, j.Name, plan, task, out)
}

// waitTask reads the task until it stops and judges it by the container that
// was asked to run.
//
// There is no deadline here, and ECS keeps none for a task either: a migration
// that hangs hangs until the pipeline gives up. A number of the tool's own
// would be a guess at how long someone else's migration takes, and a wrong one
// walks away from one that is still running.
func (d *Driver) waitTask(ctx context.Context, name string, plan *jobRun, task string, out io.Writer) error {
	timer := logging.Start("wait for task", "task", task)
	begun := time.Now()
	logs := taskLogs(plan.register, plan.container, taskID(task), d.file.Cloud.Region)

	for {
		got, err := d.ecs.DescribeTasks(ctx, &ecs.DescribeTasksInput{
			Cluster: plan.run.Cluster,
			Tasks:   []string{task},
		})
		if err != nil {
			return fmt.Errorf("task %s: %w", name, err)
		}
		if len(got.Tasks) > 0 && awssdk.ToString(got.Tasks[0].LastStatus) == "STOPPED" {
			took := time.Since(begun).Round(time.Second)
			if err := taskOutcome(got.Tasks[0], plan.container); err != nil {
				return fmt.Errorf("task %s (%s) failed after %s: %w%s",
					name, taskID(task), took, err, logs)
			}
			timer.Done()
			fmt.Fprintf(out, "task %s succeeded after %s\n", taskID(task), took)
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("task %s: gave up waiting for %s, which may still be running: %w",
				name, taskID(task), ctx.Err())
		case <-time.After(taskPoll):
		}
	}
}

func (d *Driver) prepareJobRun(ctx context.Context, j hooks.Job) (*jobRun, error) {
	// Found first, because without a network there is nothing to run in, and
	// this is the mistake that needs no call to find.
	svc, err := d.jobTarget(j)
	if err != nil {
		return nil, err
	}

	baseName := j.Base
	if baseName == "" {
		baseName = j.Name + "-base"
	}
	base, err := d.describeTaskDef(ctx, baseName)
	if err != nil {
		return nil, fmt.Errorf("base task definition %s: %w — Terraform registers "+
			"the task's shape there, the same way it does for an ecs target", baseName, err)
	}

	name, err := target.PickContainer(
		containerNames(base.ContainerDefinitions), j.Container, ecsAppContainer)
	if err != nil {
		return nil, fmt.Errorf("base task definition %s: %w", baseName, err)
	}
	img, err := image.Retag(awssdk.ToString(findContainer(base.ContainerDefinitions, name).Image), j.Version)
	if err != nil {
		return nil, err
	}
	if err := d.verifyImage(ctx, img); err != nil {
		return nil, err
	}

	// The environment is the base's: a task a hook runs is not a target, and
	// what it reads is whatever Terraform gave it.
	register := renderTaskDef(base, j.Name, name, img, nil, false, j.Command)

	// A family that does not exist yet is the first run, not a failure, and
	// any other failure to read it costs no more than registering a revision
	// that would have been the same.
	var current string
	if have, err := d.describeTaskDef(ctx, j.Name); err == nil &&
		fingerprintInput(register).equal(fingerprintTaskDef(have)) {
		current = awssdk.ToString(have.TaskDefinitionArn)
	}

	run, err := d.runInput(ctx, svc)
	if err != nil {
		return nil, err
	}
	return &jobRun{register: register, current: current, container: name, run: run}, nil
}

// jobTarget is the ECS target whose cluster and network a one-off task runs in.
//
// The hook's own service's, when it has exactly one: that is the network the
// application already reaches its database from, which is where a migration has
// to run. Anything else has to be named, rather than picked.
func (d *Driver) jobTarget(j hooks.Job) (*config.Target, error) {
	var all []*config.Target
	for _, name := range d.file.ServiceNames() {
		for _, t := range d.file.Services[name].Targets {
			if t.Type == config.TypeECS {
				all = append(all, t)
			}
		}
	}

	// The family a run registers into is the job's name. Shared with a service,
	// the next deploy would read the migration as the revision it is running.
	for _, t := range all {
		if t.Name == j.Name {
			return nil, fmt.Errorf("%s is the task definition family of the ecs target "+
				"with that name, and a task registered there would be mistaken for the "+
				"service's own — give the job a family of its own", j.Name)
		}
	}

	if j.Target != "" {
		for _, t := range all {
			if t.Name == j.Target {
				return t, nil
			}
		}
		return nil, fmt.Errorf("`target: %s` is not an ecs target in this file — it has: %s",
			j.Target, targetNames(all))
	}

	if j.Service == "" {
		return nil, fmt.Errorf("on aws a task runs in the network of an ecs service, and "+
			"a smoke test belongs to none — name one with `target`: %s", targetNames(all))
	}
	var own []*config.Target
	if svc := d.file.Services[j.Service]; svc != nil {
		for _, t := range svc.Targets {
			if t.Type == config.TypeECS {
				own = append(own, t)
			}
		}
	}
	switch len(own) {
	case 1:
		return own[0], nil
	case 0:
		return nil, fmt.Errorf("on aws a task runs in the network of an ecs service, and "+
			"%s has no ecs target — name one with `target`: %s", j.Service, targetNames(all))
	default:
		return nil, fmt.Errorf("%s has more than one ecs target, so which network the task "+
			"runs in is not obvious — name one with `target`: %s", j.Service, targetNames(own))
	}
}

// runInput is RunTask as the service would start a task of its own: its
// cluster, its subnets and security groups, its launch type or capacity
// providers. All of it is read rather than written in the config, because all
// of it is Terraform's and already declared once.
func (d *Driver) runInput(ctx context.Context, t *config.Target) (*ecs.RunTaskInput, error) {
	out, err := d.ecs.DescribeServices(ctx, &ecs.DescribeServicesInput{
		Cluster:  awssdk.String(t.Cluster),
		Services: []string{t.Name},
	})
	if err != nil {
		return nil, fmt.Errorf("describe service %s/%s: %w", t.Cluster, t.Name, err)
	}
	if len(out.Services) == 0 || awssdk.ToString(out.Services[0].Status) == "INACTIVE" {
		return nil, fmt.Errorf("service %s does not exist in cluster %s", t.Name, t.Cluster)
	}
	return runTaskInput(t.Cluster, out.Services[0]), nil
}

func runTaskInput(cluster string, svc ecstypes.Service) *ecs.RunTaskInput {
	in := &ecs.RunTaskInput{
		Cluster:              awssdk.String(cluster),
		Count:                awssdk.Int32(1),
		StartedBy:            awssdk.String("evolve-deploy"),
		NetworkConfiguration: svc.NetworkConfiguration,
		PlatformVersion:      svc.PlatformVersion,
	}
	// One or the other: RunTask refuses both, and a service that uses capacity
	// providers reports no launch type of its own.
	if len(svc.CapacityProviderStrategy) > 0 {
		in.CapacityProviderStrategy = svc.CapacityProviderStrategy
	} else {
		in.LaunchType = svc.LaunchType
	}
	return in
}

// taskOutcome judges a stopped task by the container that was asked to run.
//
// Not by the task: ECS stops every task the same way, with "Essential container
// in task exited", whether that container exited 0 or 1. The exit code is the
// verdict, and a container without one never ran — an image that would not
// pull, a secret that would not resolve — which StoppedReason explains.
func taskOutcome(task ecstypes.Task, container string) error {
	for _, c := range task.Containers {
		if awssdk.ToString(c.Name) != container {
			continue
		}
		switch {
		case c.ExitCode == nil:
			return fmt.Errorf("%s stopped without running: %s",
				container, cmp.Or(awssdk.ToString(c.Reason), awssdk.ToString(task.StoppedReason)))
		case *c.ExitCode != 0:
			return fmt.Errorf("%s exited %d", container, *c.ExitCode)
		}
		return nil
	}
	return fmt.Errorf("%s is not in the task that ran: %s",
		container, cmp.Or(awssdk.ToString(task.StoppedReason), "no reason given"))
}

// taskLogs says where a failed task's output is, as the tail of an error.
//
// Only for awslogs, where the stream name follows from the task: the prefix,
// the container, the task id. Any other driver is somewhere this cannot name.
func taskLogs(td *ecs.RegisterTaskDefinitionInput, container, id, region string) string {
	for _, c := range td.ContainerDefinitions {
		if awssdk.ToString(c.Name) != container || c.LogConfiguration == nil ||
			c.LogConfiguration.LogDriver != ecstypes.LogDriverAwslogs {
			continue
		}
		opts := c.LogConfiguration.Options
		group := opts["awslogs-group"]
		if group == "" {
			return ""
		}
		if r := opts["awslogs-region"]; r != "" {
			region = r
		}
		where := fmt.Sprintf(" — its logs are in CloudWatch, %s, log group %s", region, group)
		if prefix := opts["awslogs-stream-prefix"]; prefix != "" {
			where += fmt.Sprintf(", stream %s/%s/%s", prefix, container, id)
		}
		return where
	}
	return ""
}

func failures(fs []ecstypes.Failure) string {
	if len(fs) == 0 {
		return "RunTask returned no task and no reason"
	}
	msgs := make([]string, 0, len(fs))
	for _, f := range fs {
		msgs = append(msgs, strings.TrimSpace(awssdk.ToString(f.Reason)+" "+awssdk.ToString(f.Detail)))
	}
	return strings.Join(msgs, "; ")
}

func targetNames(ts []*config.Target) string {
	if len(ts) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		names = append(names, t.Name)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// taskID is the last segment of an ARN: for a task what the console, the log
// stream and `aws ecs describe-tasks` all call it, for a task definition
// family:revision.
func taskID(arn string) string {
	return arn[strings.LastIndex(arn, "/")+1:]
}
