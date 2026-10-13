package plan

import (
	"testing"

	"context"
	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/refs"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

func TestEnvFromPreservesNativeSecretReferences(t *testing.T) {
	d := newFakeDriver()
	d.caps[config.TypeECS] = target.Capability{NativeSecret: true}
	d.res.params["setup"] = "TOKEN=${secret:token},PARAMETER_MANAGER_VERSION=v1,OVERRIDE=${secret:missing},TEMPLATE=replace(value ${1})"
	d.res.secrets["token"] = "never-read"
	f := load(t, header+`services:
  app:
    version: v1
    envFrom: ["${param:setup}"]
    env: {OVERRIDE: explicit}
    targets: [{type: ecs, name: app, cluster: platform}]
`)
	if _, err := Build(context.Background(), f, d, nil); err != nil {
		t.Fatal(err)
	}
	env := envOf(t, d, "app")
	if env["TOKEN"].Kind != refs.Secret || env["TOKEN"].Name != "token" {
		t.Fatalf("secret = %v", env["TOKEN"])
	}
	if env["PARAMETER_MANAGER_VERSION"].Literal != "v1" || env["OVERRIDE"].Literal != "explicit" {
		t.Fatalf("environment = %v", env)
	}
	if env["TEMPLATE"].Literal != "replace(value ${1})" {
		t.Fatalf("application template was interpreted: %v", env["TEMPLATE"])
	}
	for _, read := range d.res.reads {
		if read == "${secret:token}" {
			t.Fatal("secret payload was read")
		}
	}
}

func TestEnvFromReferencesRespectResolveDeny(t *testing.T) {
	d := newFakeDriver()
	d.res.params["setup"] = "TOKEN=${secret:token}"
	d.res.secrets["token"] = "never-read"
	f := load(t, header+`refs: {resolve: deny}
services:
  app:
    version: v1
    envFrom: ["${param:setup}"]
    targets: [{type: lambda, name: app, code: {bucket: b, key: k.zip}}]
`)
	if _, err := Build(context.Background(), f, d, nil); err == nil {
		t.Fatal("secret resolution was allowed")
	}
}
