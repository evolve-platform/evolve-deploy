package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadStrategy(t *testing.T, body string) (*File, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tst.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path, "tst")
}

const azureHeader = `
cloud:
  provider: azure
  subscription: sub
  resource_group: rg
`

func TestStrategyDefaultsToDirect(t *testing.T) {
	f, err := loadStrategy(t, azureHeader+`
services:
  site:
    version: abc1234
    type: container-app
`)
	if err != nil {
		t.Fatal(err)
	}

	s := f.Services["site"].Strategy
	if s == nil {
		t.Fatal("the effective strategy is nil; the rest of the tool may not check")
	}
	if s.Type != StrategyDirect {
		t.Errorf("type = %q, want direct", s.Type)
	}
	if s.IsBlueGreen() {
		t.Error("a file that says nothing must not stage anything")
	}
	if got := strings.Join(s.Labels, ","); got != "blue,green" {
		t.Errorf("labels = %q", got)
	}
}

// The file's block is the default and a service overrides it field by field, so
// one service can opt out without restating the rest.
func TestStrategyMergesPerField(t *testing.T) {
	f, err := loadStrategy(t, azureHeader+`
strategy:
  type: blue-green
  smoke: [ "curl -fsS {{.url}}/healthz" ]
  labels: [ one, two ]

services:
  inherits:
    version: abc1234
    type: container-app

  quiet:
    version: abc1234
    type: container-app
    strategy:
      smoke: []

  opts-out:
    version: abc1234
    type: container-app
    strategy:
      type: direct
`)
	if err != nil {
		t.Fatal(err)
	}

	inherits := f.Services["inherits"].Strategy
	if !inherits.IsBlueGreen() || len(inherits.Smoke) != 1 {
		t.Errorf("inherits = %+v", inherits)
	}
	if got := strings.Join(inherits.Labels, ","); got != "one,two" {
		t.Errorf("labels = %q, want the file's", got)
	}

	// An empty list is not an absent one: this is how "no smoke test here" is
	// said without repeating everything else.
	quiet := f.Services["quiet"].Strategy
	if !quiet.IsBlueGreen() {
		t.Error("quiet should still be blue-green")
	}
	if len(quiet.Smoke) != 0 {
		t.Errorf("smoke = %v, want none", quiet.Smoke)
	}

	if f.Services["opts-out"].Strategy.IsBlueGreen() {
		t.Error("opts-out asked for direct")
	}
}

// The strategy reaches the driver through the target, because a driver is
// handed targets and needs the label names.
func TestStrategyReachesEveryTarget(t *testing.T) {
	f, err := loadStrategy(t, azureHeader+`
strategy:
  type: blue-green

services:
  discover:
    version: abc1234
    targets:
      - { type: container-app,     name: discover }
      - { type: container-app-job, name: discover-sync }
`)
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range f.Services["discover"].Targets {
		if !tg.Strategy.IsBlueGreen() {
			t.Errorf("%s did not get the strategy", tg.Label())
		}
	}
}

func TestStrategyValidation(t *testing.T) {
	cases := []struct{ name, body, wantErr string }{{
		name:    "an unknown type",
		body:    "strategy:\n  type: canary\n",
		wantErr: "not one of direct, blue-green",
	}, {
		name:    "one label",
		body:    "strategy:\n  type: blue-green\n  labels: [ blue ]\n",
		wantErr: "exactly two names",
	}, {
		name:    "two labels with the same name",
		body:    "strategy:\n  type: blue-green\n  labels: [ blue, blue ]\n",
		wantErr: "both sides are called",
	}, {
		name:    "smoke on a direct release has nothing to run against",
		body:    "strategy:\n  type: direct\n  smoke: [ true ]\n",
		wantErr: "needs a staged side",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadStrategy(t, azureHeader+c.body+`
services:
  site:
    version: abc1234
    type: container-app
`)
			if err == nil {
				t.Fatalf("no error, want one mentioning %q", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error = %v", err)
			}
		})
	}
}

// A bad value at file level is one mistake and should read as one, not as one
// message per service that inherited it.
func TestFileLevelStrategyIsReportedOnce(t *testing.T) {
	_, err := loadStrategy(t, azureHeader+`
strategy:
  type: canary

services:
  a: { version: abc1234, type: container-app }
  b: { version: abc1234, type: container-app }
  c: { version: abc1234, type: container-app }
`)
	if err == nil {
		t.Fatal("want an error")
	}
	if n := strings.Count(err.Error(), "not one of direct, blue-green"); n != 1 {
		t.Errorf("the same mistake is reported %d times:\n%v", n, err)
	}
}

// The tool writes this one itself and the environment diff ignores it, so a
// config that also set it would be changing something nothing would report.
func TestSideVariableCannotBeSetByHand(t *testing.T) {
	_, err := loadStrategy(t, azureHeader+`
services:
  site:
    version: abc1234
    type: container-app
    env:
      EVOLVE_DEPLOY_SIDE: green
`)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "written by the tool") {
		t.Errorf("error = %v", err)
	}
}

func TestSideEnvIsCarriedPerSide(t *testing.T) {
	f, err := loadStrategy(t, azureHeader+`
strategy:
  type: blue-green
services:
  graphql-gateway:
    version: abc1234
    type: container-app
    strategy:
      env:
        blue:
          HIVE_CDN_ENDPOINT: https://cdn/artifacts/v1/tst-blue
          HIVE_CDN_KEY: ${secret:hive-cdn-blue}
        green:
          HIVE_CDN_ENDPOINT: https://cdn/artifacts/v1/tst-green
          HIVE_CDN_KEY: ${secret:hive-cdn-green}
`)
	if err != nil {
		t.Fatal(err)
	}

	c := f.Services["graphql-gateway"]
	if got := c.Strategy.Env["green"]["HIVE_CDN_ENDPOINT"]; got != "https://cdn/artifacts/v1/tst-green" {
		t.Errorf("green endpoint = %q", got)
	}
	if got := strings.Join(c.Strategy.SideEnvNames(), ","); got != "HIVE_CDN_ENDPOINT,HIVE_CDN_KEY" {
		t.Errorf("managed names = %q", got)
	}
	// The driver is handed targets, not services, and needs the same view.
	if c.Targets[0].Strategy.Env["blue"]["HIVE_CDN_KEY"] != "${secret:hive-cdn-blue}" {
		t.Error("the target did not get the service's side environment")
	}
}

// A variable one side sets and the other does not is inherited from the other
// side's revision rather than unset, so it is refused rather than merged.
func TestSideEnvMustNameTheSameVariablesOnBothSides(t *testing.T) {
	_, err := loadStrategy(t, azureHeader+`
strategy:
  type: blue-green
services:
  site:
    version: abc1234
    type: container-app
    strategy:
      env:
        blue:
          GATEWAY_URL: https://gw---blue.example.com
        green:
          GATEWAY_URL: https://gw---green.example.com
          EXTRA: only-here
`)
	if err == nil {
		t.Fatal("a variable set on one side only was accepted")
	}
	if !strings.Contains(err.Error(), "EXTRA") || !strings.Contains(err.Error(), "blue") {
		t.Errorf("the error does not say which variable or which side: %v", err)
	}
}

func TestSideEnvRejectsUnknownSidesAndDirect(t *testing.T) {
	_, err := loadStrategy(t, azureHeader+`
strategy:
  type: blue-green
services:
  site:
    version: abc1234
    type: container-app
    strategy:
      env:
        purple:
          GATEWAY_URL: https://gw.example.com
`)
	if err == nil || !strings.Contains(err.Error(), "purple") {
		t.Errorf("an unknown side was accepted: %v", err)
	}

	_, err = loadStrategy(t, azureHeader+`
services:
  site:
    version: abc1234
    type: container-app
    strategy:
      type: direct
      env:
        blue:
          GATEWAY_URL: https://gw.example.com
`)
	if err == nil || !strings.Contains(err.Error(), "direct") {
		t.Errorf("per-side environment on a direct service was accepted: %v", err)
	}
}

// The tool writes the side itself, and the diff ignores it, so a config that
// also set it would be changing something nothing would ever report.
func TestSideEnvRejectsTheSideVariable(t *testing.T) {
	_, err := loadStrategy(t, azureHeader+`
strategy:
  type: blue-green
services:
  site:
    version: abc1234
    type: container-app
    strategy:
      env:
        blue:
          EVOLVE_DEPLOY_SIDE: blue
        green:
          EVOLVE_DEPLOY_SIDE: green
`)
	if err == nil || !strings.Contains(err.Error(), SideEnvVar) {
		t.Errorf("the tool's own variable was accepted: %v", err)
	}
}

// Between two staged services there is nothing to order, so saying so is a
// mistake rather than a no-op: it would only serialise the staging.
func TestDependsOnIsRefusedBetweenBlueGreenServices(t *testing.T) {
	_, err := loadStrategy(t, azureHeader+`
strategy:
  type: blue-green
services:
  purchase:
    version: abc1234
    type: container-app
  site:
    version: abc1234
    type: container-app
    depends_on: [purchase]
`)
	if err == nil {
		t.Fatal("depends_on between two blue-green services was accepted")
	}
	if !strings.Contains(err.Error(), "both are blue-green") {
		t.Errorf("the error does not explain why: %v", err)
	}
}

// One `direct` end and the ordering is real again: there is no other side to
// address, so that service genuinely goes live before or after.
func TestDependsOnSurvivesWhenOneEndIsDirect(t *testing.T) {
	f, err := loadStrategy(t, azureHeader+`
strategy:
  type: blue-green
services:
  purchase:
    version: abc1234
    type: container-app
    strategy:
      type: direct
  site:
    version: abc1234
    type: container-app
    depends_on: [purchase]
`)
	if err != nil {
		t.Fatalf("a mixed edge was refused: %v", err)
	}
	if got := strings.Join(f.Services["site"].DependsOn, ","); got != "purchase" {
		t.Errorf("depends_on = %q", got)
	}
}
