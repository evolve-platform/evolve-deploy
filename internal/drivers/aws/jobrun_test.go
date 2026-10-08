package aws

import (
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/hooks"
)

func jobFile() *config.File {
	ecs := func(name string) *config.Target {
		return &config.Target{Type: config.TypeECS, Name: name, Cluster: "platform"}
	}
	return &config.File{
		Cloud: config.CloudConfig{Region: "eu-west-1"},
		Services: map[string]*config.Service{
			"ask":      {Name: "ask", Targets: []*config.Target{ecs("ask-web")}},
			"purchase": {Name: "purchase", Targets: []*config.Target{ecs("purchase"), ecs("purchase-worker")}},
			"events":   {Name: "events", Targets: []*config.Target{{Type: config.TypeLambda, Name: "events"}}},
		},
	}
}

func TestATaskRunsInTheNetworkOfItsOwnService(t *testing.T) {
	d := &Driver{file: jobFile()}

	got, err := d.jobTarget(hooks.Job{Name: "ask-migrate", Service: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "ask-web" {
		t.Errorf("borrowed the network of %s", got.Name)
	}

	got, err = d.jobTarget(hooks.Job{Name: "ask-migrate", Service: "events", Target: "purchase-worker"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "purchase-worker" {
		t.Errorf("`target` named purchase-worker, and it used %s", got.Name)
	}
}

func TestATaskWhoseNetworkIsNotObviousIsRefused(t *testing.T) {
	d := &Driver{file: jobFile()}
	for _, c := range []struct {
		job  hooks.Job
		want string
	}{
		{hooks.Job{Name: "m", Service: "purchase"}, "more than one ecs target"},
		{hooks.Job{Name: "m", Service: "events"}, "events has no ecs target"},
		{hooks.Job{Name: "m"}, "a smoke test belongs to none"},
		{hooks.Job{Name: "m", Service: "ask", Target: "nope"}, "`target: nope` is not an ecs target"},
		// The job's family is a service's: a migration registered there is
		// what the next deploy would read as the service's running revision.
		{hooks.Job{Name: "ask-web", Service: "ask"}, "family of the ecs target"},
	} {
		_, err := d.jobTarget(c.job)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: error was %v, want it to mention %q", c.job, err, c.want)
		}
	}
}

func TestATaskStartsTheWayItsServiceDoes(t *testing.T) {
	network := &ecstypes.NetworkConfiguration{AwsvpcConfiguration: &ecstypes.AwsVpcConfiguration{
		Subnets: []string{"subnet-a"}, SecurityGroups: []string{"sg-app"},
	}}

	in := runTaskInput("platform", ecstypes.Service{
		LaunchType: ecstypes.LaunchTypeFargate, NetworkConfiguration: network,
	})
	if in.LaunchType != ecstypes.LaunchTypeFargate || in.NetworkConfiguration != network {
		t.Errorf("launch type %q, network %v", in.LaunchType, in.NetworkConfiguration)
	}

	// RunTask refuses a launch type next to a capacity provider strategy.
	in = runTaskInput("platform", ecstypes.Service{
		CapacityProviderStrategy: []ecstypes.CapacityProviderStrategyItem{
			{CapacityProvider: awssdk.String("FARGATE_SPOT")},
		},
	})
	if in.LaunchType != "" || len(in.CapacityProviderStrategy) != 1 {
		t.Errorf("launch type %q next to %d capacity providers", in.LaunchType, len(in.CapacityProviderStrategy))
	}
}

func TestATaskIsJudgedByItsContainersExitCode(t *testing.T) {
	stopped := func(c ecstypes.Container) ecstypes.Task {
		return ecstypes.Task{
			// What ECS says about every task, passed or failed.
			StoppedReason: awssdk.String("Essential container in task exited"),
			Containers: []ecstypes.Container{
				{Name: awssdk.String("reverse-proxy"), ExitCode: awssdk.Int32(137)},
				c,
			},
		}
	}

	if err := taskOutcome(stopped(ecstypes.Container{
		Name: awssdk.String("uwsgi"), ExitCode: awssdk.Int32(0),
	}), "uwsgi"); err != nil {
		t.Errorf("exit 0 failed: %v — a sidecar's exit code is not the verdict", err)
	}

	err := taskOutcome(stopped(ecstypes.Container{
		Name: awssdk.String("uwsgi"), ExitCode: awssdk.Int32(1),
	}), "uwsgi")
	if err == nil || !strings.Contains(err.Error(), "uwsgi exited 1") {
		t.Errorf("error was %v", err)
	}

	err = taskOutcome(stopped(ecstypes.Container{
		Name:   awssdk.String("uwsgi"),
		Reason: awssdk.String("CannotPullContainerError: manifest unknown"),
	}), "uwsgi")
	if err == nil || !strings.Contains(err.Error(), "stopped without running: CannotPullContainerError") {
		t.Errorf("error was %v", err)
	}
}

func TestAFailedTaskSaysWhereItsLogsAre(t *testing.T) {
	td := &ecs.RegisterTaskDefinitionInput{ContainerDefinitions: []ecstypes.ContainerDefinition{{
		Name: awssdk.String("uwsgi"),
		LogConfiguration: &ecstypes.LogConfiguration{
			LogDriver: ecstypes.LogDriverAwslogs,
			Options:   map[string]string{"awslogs-group": "/ecs/ask", "awslogs-stream-prefix": "ecs"},
		},
	}}}

	got := taskLogs(td, "uwsgi", "0f1e2d", "eu-west-1")
	if !strings.Contains(got, "eu-west-1, log group /ecs/ask, stream ecs/uwsgi/0f1e2d") {
		t.Errorf("logs = %q", got)
	}
	if got := taskLogs(td, "reverse-proxy", "0f1e2d", "eu-west-1"); got != "" {
		t.Errorf("named logs for a container it has no configuration for: %q", got)
	}
}
