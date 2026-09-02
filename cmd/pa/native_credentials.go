package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// nativeCredentialProvider resolves the DSH credential reference (the
// environment-variable name exposed by the provider catalog) back to Shutu's
// provider id. Production values live behind the dedicated credential Vault;
// the llm.key.<provider> setting branch is a compatibility fallback only for
// embedders that supply no Vault. The environment remains read-only and
// therefore continues to win when present.
func (a *app) nativeCredentialProvider(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	for _, provider := range builtinProviders {
		if providerEnv(provider.id) == ref {
			return provider.id, nil
		}
	}
	a.providerMu.RLock()
	customProviders := append([]customProviderProfile(nil), a.customProviders...)
	a.providerMu.RUnlock()
	for _, provider := range customProviders {
		if llmKeyEnv(provider.ID) == ref {
			return provider.ID, nil
		}
	}
	return "", fmt.Errorf("credential reference %q is not registered", ref)
}

func (a *app) nativeCredentialSet(ctx context.Context, ref, value string) error {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	if a.store == nil {
		return errors.New("credential store is not available")
	}
	provider, err := a.nativeCredentialProvider(ref)
	if err != nil {
		return err
	}
	if strings.TrimSpace(value) == "" {
		return errors.New("credential value is empty")
	}
	if os.Getenv(ref) != "" {
		return errors.New("credential is provided by the environment and is read-only")
	}
	old, hadOld := a.credentialOverride(provider)
	if a.credentials != nil {
		if err := a.credentials.Set(ctx, ref, value); err != nil {
			return err
		}
	} else if err := a.store.SetSetting(ctx, "llm.key."+provider, value); err != nil {
		// Lightweight app fixtures may not install the credential vault; retain
		// the source-compatible fallback only for those embedders/tests.
		return err
	}
	a.setCredentialOverride(provider, value)
	if a.hasLLMRegistry() {
		if err := a.registerLLM(); err != nil {
			return errors.Join(err, a.rollbackCredentialOverride(ctx, provider, old, hadOld))
		}
	}
	return nil
}

func (a *app) nativeCredentialUnset(ctx context.Context, ref string) error {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	if a.store == nil {
		return errors.New("credential store is not available")
	}
	provider, err := a.nativeCredentialProvider(ref)
	if err != nil {
		return err
	}
	if a.credentials != nil {
		if err := a.credentials.Unset(ctx, ref); err != nil {
			return err
		}
	} else if err := a.store.DeleteSetting(ctx, "llm.key."+provider); err != nil {
		return err
	}
	old, hadOld := a.credentialOverride(provider)
	a.setCredentialOverride(provider, "")
	if a.hasLLMRegistry() {
		if err := a.registerLLM(); err != nil {
			return errors.Join(err, a.rollbackCredentialOverride(ctx, provider, old, hadOld))
		}
	}
	return nil
}

// credentialOverride snapshots the process-local persisted-key overlay. The
// overlay is deliberately kept separate from environment credentials: an
// unset override must reveal the environment again, while a failed live
// provider rebuild must restore exactly the previous overlay.
func (a *app) credentialOverride(provider string) (string, bool) {
	a.providerMu.RLock()
	defer a.providerMu.RUnlock()
	if a.llmKeys == nil {
		return "", false
	}
	value, ok := a.llmKeys[provider]
	return value, ok
}

func (a *app) setCredentialOverride(provider, value string) {
	a.providerMu.Lock()
	defer a.providerMu.Unlock()
	if a.llmKeys == nil {
		a.llmKeys = make(map[string]string)
	}
	if value == "" {
		delete(a.llmKeys, provider)
		return
	}
	a.llmKeys[provider] = value
}

// rollbackCredentialOverride restores both durable and in-memory state after
// registerLLM rejects a credential change. The second registerLLM attempt is
// intentionally made while controlMu is held, so another legacy turn cannot observe a
// half-restored provider selection.
func (a *app) rollbackCredentialOverride(ctx context.Context, provider, old string, hadOld bool) error {
	var first error
	ref := llmKeyEnv(provider)
	if a.credentials != nil {
		if hadOld {
			if err := a.credentials.Set(ctx, ref, old); err != nil {
				first = err
			}
		} else if err := a.credentials.Unset(ctx, ref); err != nil {
			first = err
		}
	} else if hadOld {
		if err := a.store.SetSetting(ctx, "llm.key."+provider, old); err != nil {
			first = err
		}
	} else if err := a.store.DeleteSetting(ctx, "llm.key."+provider); err != nil {
		first = err
	}
	if hadOld {
		a.setCredentialOverride(provider, old)
	} else {
		a.setCredentialOverride(provider, "")
	}
	if err := a.registerLLM(); err != nil {
		if first != nil {
			return errors.Join(first, fmt.Errorf("restore provider registry: %w", err))
		}
		return fmt.Errorf("restore provider registry: %w", err)
	}
	return first
}

func (a *app) hasLLMRegistry() bool {
	a.llmMu.RLock()
	defer a.llmMu.RUnlock()
	return a.llmReg != nil
}
