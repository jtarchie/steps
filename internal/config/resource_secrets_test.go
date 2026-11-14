package config

import (
	"reflect"
	"slices"
	"testing"
)

// A secret's name joins env: in what a resource keys on, so naming one more variable moves the key; the reference never does, since it decides access to a version and not what the version is.
func TestResourceHashedEnvIsNamesOnly(t *testing.T) {
	t.Parallel()

	r := Resource{Env: []string{"A"}, Secrets: map[string]string{"PUBLISH_AS": "((ci:x))"}}
	other := Resource{Env: []string{"A"}, Secrets: map[string]string{"PUBLISH_AS": "((ci:y))"}}

	if !slices.Equal(r.HashedEnv(), []string{"A", "PUBLISH_AS"}) {
		t.Errorf("HashedEnv = %v, want env and the secret's name", r.HashedEnv())
	}

	if !slices.Equal(r.HashedEnv(), other.HashedEnv()) {
		t.Error("a different reference under the same name changed what the resource keys on")
	}
}

// SecretUses is what validate --live resolves in the daemon's stead, so it has to name every (reference, where) the daemon would: a get that aliases its resource resolves the resource's secrets, not a resource named by the alias, and a passed: get is polled the way a trigger: true one is.
func TestSecretUsesFollowAliasesAndPassedGets(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
defaults:
  preflight:
    disabled: true

resource_types:
- name: dummy
  config: {check: "echo []", in: "true", out: "true"}
resources:
- name: feed
  type: dummy
  source: {}
  secrets:
    TOKEN: ((ci:feed_token))
- name: gate
  type: dummy
  source: {}
  secrets:
    TOKEN: ((ci:gate_token))
jobs:
- name: build
  plan:
  - get: alias
    resource: feed
    trigger: true
  - get: gate
- name: downstream
  plan:
  - get: gate
    passed: [build]
  - put: out
    resource: feed
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	want := []SecretUse{
		{Ref: SecretRef{Source: "ci", Name: "feed_token"}, Resource: "feed", Polled: true},
		{Ref: SecretRef{Source: "ci", Name: "feed_token"}, Job: "build", Resource: "feed"},
		{Ref: SecretRef{Source: "ci", Name: "feed_token"}, Job: "downstream", Resource: "feed"},
		{Ref: SecretRef{Source: "ci", Name: "gate_token"}, Resource: "gate", Polled: true},
		{Ref: SecretRef{Source: "ci", Name: "gate_token"}, Job: "build", Resource: "gate"},
		{Ref: SecretRef{Source: "ci", Name: "gate_token"}, Job: "downstream", Resource: "gate"},
	}

	if got := cfg.SecretUses(); !reflect.DeepEqual(got, want) {
		t.Errorf("SecretUses = %+v, want %+v", got, want)
	}
}

// A webhook's secret is resolved when a delivery arrives, under no job, and a get of it resolves nothing: validate --live must ask for exactly the lookup the daemon makes.
func TestAWebhooksSecretIsUsedOutsideAnyRun(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
resources:
- name: push
  type: webhook
  source: { provider: github, secret_env: HOOK }
  secrets: { HOOK: ((ci:hook)) }
jobs:
- name: build
  plan:
  - get: push
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	want := []SecretUse{{Ref: SecretRef{Source: "ci", Name: "hook"}, Resource: "push", Polled: true}}

	if got := cfg.SecretUses(); !reflect.DeepEqual(got, want) {
		t.Errorf("SecretUses = %+v, want %+v", got, want)
	}
}

// An oauth server's client secret is fetched by a login and never by a run: an export may reach the server, and no job lists the reference among what it resolves.
func TestAnOAuthClientSecretIsNoRunsToResolve(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
mcp_servers:
- name: tracker
  endpoint: https://example.com/mcp
  auth: { type: oauth, client_id: app, client_secret_env: CLIENT, secrets: { CLIENT: ((ci:client)) } }
mcp_exports:
- name: support
  access: '"acme/support" in Groups'
  tools:
  - mcp: tracker
    tool: read_items
agents:
- name: reader
  source: { model: openai/test-model, api_key_env: KEY }
  tools:
  - mcp: tracker
    tool: read_items
jobs:
- name: read
  plan:
  - agent: reader
    messages: [hi]
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if got := cfg.SecretUses(); len(got) != 0 {
		t.Errorf("SecretUses = %+v, want none: the client secret is a login's", got)
	}

	if got := cfg.SecretRefs(); len(got) != 1 {
		t.Errorf("SecretRefs = %+v, want the one reference, so a set still checks its source exists", got)
	}
}
