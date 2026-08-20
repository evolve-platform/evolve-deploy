package aws

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"

	"github.com/evolve-platform/evolve-deploy/internal/target"
	"github.com/evolve-platform/evolve-deploy/internal/tmpl"
)

const lambdaUpdateTimeout = 5 * time.Minute

// versionVar records which version a function runs.
//
// Every other target carries its version in an image tag that can be read back.
// A Lambda carries a zip whose only identity in the API is a content hash, so
// without this the tool could not tell whether a function is already up to date
// and would redeploy on every run. It lives in the environment rather than in a
// tag because tags on the function belong to Terraform, and a second writer
// there means the next apply strips it.
const versionVar = "EVOLVE_DEPLOY_VERSION"

type lambdaPayload struct {
	bucket, key string
	env         map[string]string

	// For rollback: the version that was running, from which the previous
	// object key can be derived.
	previousVersion string
	previousEnv     map[string]string
	keyTemplate     string
}

// planLambda works out the code and environment a function should have.
//
// Lambda is the one target where references cannot be handed to the platform —
// its environment variables are literal strings — so by the time planning gets
// here every value has already been read and turned into a literal, or the plan
// failed under refs.resolve: deny.
func (d *Driver) planLambda(ctx context.Context, want *target.Desired) (*target.Change, error) {
	t := want.Target

	key, err := tmpl.Render(t.Code.Key, map[string]string{
		"version": want.Version,
		"name":    t.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("code.key: %w", err)
	}
	if err := d.verifyObject(ctx, t.Code.Bucket, key); err != nil {
		return nil, err
	}

	cfg, err := d.lambda.GetFunctionConfiguration(ctx, &lambda.GetFunctionConfigurationInput{
		FunctionName: awssdk.String(t.Name),
	})
	if err != nil {
		return nil, fmt.Errorf("function %s: %w", t.Name, err)
	}

	currentEnv := map[string]string{}
	if cfg.Environment != nil {
		for k, v := range cfg.Environment.Variables {
			currentEnv[k] = v
		}
	}
	from := currentEnv[versionVar]

	desiredEnv := make(map[string]string, len(want.Env)+1)
	for _, e := range want.Env {
		// Anything still a reference here would be written out as "${secret:x}"
		// and break the function at runtime. Planning should have resolved it,
		// so this is a guard against a driver capability table that lies.
		if e.Value.IsRef() {
			return nil, fmt.Errorf(
				"env.%s: %s cannot be given to a lambda as a reference", e.Name, e.Value.Raw)
		}
		desiredEnv[e.Name] = e.Value.Literal
	}
	desiredEnv[versionVar] = want.Version

	if from == want.Version && sameEnv(currentEnv, desiredEnv) {
		return nil, nil
	}

	added, changed, removed := diffMaps(currentEnv, desiredEnv)
	return &target.Change{
		Service:     want.Service,
		Target:      t,
		FromVersion: from,
		ToVersion:   want.Version,
		Reason:      lambdaReason(from, want.Version),
		EnvAdded:    added,
		EnvChanged:  changed,
		EnvRemoved:  removed,
		Payload: &lambdaPayload{
			bucket:          t.Code.Bucket,
			key:             key,
			env:             desiredEnv,
			previousVersion: from,
			previousEnv:     currentEnv,
			keyTemplate:     t.Code.Key,
		},
	}, nil
}

// applyLambda updates code and configuration, which Lambda will not accept at
// the same time: each update leaves the function in Pending until it settles.
func (d *Driver) applyLambda(ctx context.Context, ch *target.Change) error {
	p := ch.Payload.(*lambdaPayload)
	name := ch.Target.Name

	if err := d.pushLambda(ctx, name, p.bucket, p.key, p.env); err != nil {
		return d.rollbackLambda(ctx, ch, p, err)
	}
	return nil
}

func (d *Driver) pushLambda(ctx context.Context, name, bucket, key string, env map[string]string) error {
	target.Status(ctx, "the new code to be taken")
	if _, err := d.lambda.UpdateFunctionCode(ctx, &lambda.UpdateFunctionCodeInput{
		FunctionName: awssdk.String(name),
		S3Bucket:     awssdk.String(bucket),
		S3Key:        awssdk.String(key),
	}); err != nil {
		return fmt.Errorf("update code: %w", err)
	}
	if err := d.waitLambda(ctx, name); err != nil {
		return err
	}

	if _, err := d.lambda.UpdateFunctionConfiguration(ctx, &lambda.UpdateFunctionConfigurationInput{
		FunctionName: awssdk.String(name),
		Environment:  &lambdatypes.Environment{Variables: env},
	}); err != nil {
		return fmt.Errorf("update configuration: %w", err)
	}
	return d.waitLambda(ctx, name)
}

func (d *Driver) waitLambda(ctx context.Context, name string) error {
	target.Status(ctx, "the function to settle")
	waiter := lambda.NewFunctionUpdatedV2Waiter(d.lambda)
	if err := waiter.Wait(ctx, &lambda.GetFunctionInput{
		FunctionName: awssdk.String(name),
	}, lambdaUpdateTimeout); err != nil {
		return fmt.Errorf("waiting for %s to settle: %w", name, err)
	}
	return nil
}

// rollbackLambda puts the previous zip and environment back.
//
// Lambda has no health check to fail, so there is no platform-side rollback to
// lean on the way ECS has its circuit breaker. The previous key is derived from
// the version marker, which is the second reason that marker exists.
func (d *Driver) rollbackLambda(
	ctx context.Context, ch *target.Change, p *lambdaPayload, cause error,
) error {
	if p.previousVersion == "" {
		return fmt.Errorf("%w (no previous version recorded, so nothing to roll back to)", cause)
	}

	key, err := tmpl.Render(p.keyTemplate, map[string]string{
		"version": p.previousVersion,
		"name":    ch.Target.Name,
	})
	if err != nil {
		return errors.Join(cause, fmt.Errorf("rollback also failed: %w", err))
	}
	if err := d.pushLambda(ctx, ch.Target.Name, p.bucket, key, p.previousEnv); err != nil {
		return errors.Join(cause,
			fmt.Errorf("rollback to %s also failed: %w", p.previousVersion, err))
	}
	return fmt.Errorf("rolled back to %s: %w", p.previousVersion, cause)
}

// revertLambda restores the previous zip and environment after a sibling target
// of the same service failed.
func (d *Driver) revertLambda(ctx context.Context, ch *target.Change) error {
	p := ch.Payload.(*lambdaPayload)
	if p.previousVersion == "" {
		return fmt.Errorf("no previous version recorded for %s", ch.Target.Name)
	}
	key, err := tmpl.Render(p.keyTemplate, map[string]string{
		"version": p.previousVersion,
		"name":    ch.Target.Name,
	})
	if err != nil {
		return err
	}
	return d.pushLambda(ctx, ch.Target.Name, p.bucket, key, p.previousEnv)
}

func lambdaReason(from, to string) string {
	if from != to {
		return fmt.Sprintf("version %s -> %s", orNone(from), to)
	}
	return "environment changed"
}

func sameEnv(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		if bv, ok := b[k]; !ok || av != bv {
			return false
		}
	}
	return true
}

func diffMaps(have, want map[string]string) (added, changed, removed []string) {
	for k, wv := range want {
		hv, ok := have[k]
		switch {
		case !ok:
			added = append(added, k)
		case hv != wv:
			changed = append(changed, k)
		}
	}
	for k := range have {
		if _, ok := want[k]; !ok {
			removed = append(removed, k)
		}
	}
	sort.Strings(added)
	sort.Strings(changed)
	sort.Strings(removed)
	return added, changed, removed
}
