// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/phasecontrol"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtimecanary"
)

type phaseCanaryCompletion struct {
	resp phasecontrol.Response
}

func (r *runner) pollRuntimeCanaryRequest(
	ctx context.Context,
	sl *ledger.Slot,
	req phasecontrol.Request,
	digest string,
) (phasecontrol.Response, bool) {
	if r.phaseCanaries == nil {
		r.phaseCanaries = make(map[string]<-chan phaseCanaryCompletion)
	}
	key := sl.PhaseID + ":" + req.ID
	if ch, ok := r.phaseCanaries[key]; ok {
		select {
		case completed := <-ch:
			delete(r.phaseCanaries, key)
			return completed.resp, true
		default:
			return phasecontrol.Response{}, false
		}
	}

	ch := make(chan phaseCanaryCompletion, 1)
	r.phaseCanaries[key] = ch
	go func() {
		result := r.executeRuntimeCanary(ctx, sl, req.Runtime)
		state := phasecontrol.ResponseFailed
		if result.OK {
			state = phasecontrol.ResponseApplied
		} else if result.Kind == "configuration" {
			state = phasecontrol.ResponseRejected
		}
		detail := req.Runtime + ": " + result.Detail
		ch <- phaseCanaryCompletion{resp: phaseResponse(req, digest, state, detail)}
	}()
	return phasecontrol.Response{}, false
}

func (r *runner) executeRuntimeCanary(ctx context.Context, sl *ledger.Slot, runtimeName string) runtimecanary.Result {
	if err := phasecontrol.ValidateRuntimeName(runtimeName); err != nil {
		return runtimecanary.Result{Runtime: runtimeName, Kind: "configuration", Detail: "invalid target runtime"}
	}
	rt, ok := runtimeForName(runtimeName)
	if !ok {
		return runtimecanary.Result{Runtime: runtimeName, Kind: "configuration", Detail: "target runtime is not registered"}
	}
	if !runtimeEnabled(r.cfg, runtimeName) {
		return runtimecanary.Result{Runtime: runtimeName, Kind: "configuration", Detail: "target runtime is not enabled for this project"}
	}

	ra := r.rec.AccountFor(runtimeName)
	profile := runtime.Profile{Name: r.rec.AccountProfile, ConfigDir: ra.ConfigDir}
	authMode := ra.EffectiveAuthMode()
	var credential, credentialEnvVar string
	var verify runtimecanary.VerifyFunc
	switch authMode {
	case registry.AuthModeSubscription:
		verify = func(checkCtx context.Context) (string, error) {
			return rt.VerifyIdentity(checkCtx, profile, ra.ExpectedIdentity)
		}
	case registry.AuthModeAPIKey, registry.AuthModeOAuthToken:
		if rt.Name() != "claude" {
			return runtimecanary.Result{Runtime: runtimeName, Kind: "configuration", Detail: "target runtime requires its native authenticated profile"}
		}
		authSpec := account.AuthSpec{
			Mode:                account.AuthMode(authMode),
			ExpectedIdentity:    ra.ExpectedIdentity,
			Credential:          ra.Credential,
			IdentityFingerprint: ra.IdentityFingerprint,
		}
		var err error
		credentialEnvVar, credential, err = account.ResolveCredential(ctx, account.AuthMode(authMode), ra.Credential)
		if err != nil {
			return runtimecanary.Result{Runtime: runtimeName, Kind: "configuration", Detail: "target runtime credential is unavailable"}
		}
		verify = func(checkCtx context.Context) (string, error) {
			id, err := account.VerifyAuth(checkCtx, account.Profile{Name: profile.Name, ConfigDir: profile.ConfigDir}, authSpec)
			if err != nil {
				return "", err
			}
			return id.Email, nil
		}
	default:
		return runtimecanary.Result{Runtime: runtimeName, Kind: "configuration", Detail: "target runtime has an unsupported authentication mode"}
	}

	models := make(runtime.ModelMap, len(rt.ModelMap()))
	for tier, model := range rt.ModelMap() {
		models[tier] = model
	}
	if rc, configured := r.cfg.Runtimes[runtimeName]; configured {
		for tier, model := range rc.ModelMap {
			models[tier] = model
		}
	}
	model := models[runtime.TierStandard]
	runCanary := r.runtimeCanaryRun
	if runCanary == nil {
		runCanary = runtimecanary.Run
	}
	result := runCanary(ctx, runtimecanary.Options{
		Runtime:          rt,
		RepoRoot:         r.rec.Root,
		Worktree:         sl.Worktree,
		ScratchDir:       r.store.PhaseDir(r.run.RunID, sl.PhaseID),
		Model:            model,
		Profile:          profile,
		Billing:          runtime.BillingSubscription,
		ProxyBaseURL:     r.rec.ProxyBaseURL(),
		EnvPassthrough:   ra.EnvPassthrough,
		Credential:       credential,
		CredentialEnvVar: credentialEnvVar,
		Verify:           verify,
	})
	if result.Runtime == "" {
		result.Runtime = runtimeName
	}
	return result
}
