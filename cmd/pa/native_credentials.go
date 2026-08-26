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
// provider id. Values are stored in the existing llm.key.<provider> layer; the
// environment remains read-only and therefore continues to win when present.
func (a *app) nativeCredentialProvider(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	for _, provider := range builtinProviders {
		if providerEnv(provider.id) == ref {
			return provider.id, nil
		}
	}
	for _, provider := range a.customProviders {
		if llmKeyEnv(provider.ID) == ref {
			return provider.ID, nil
		}
	}
	return "", fmt.Errorf("credential reference %q is not registered", ref)
}

func (a *app) nativeCredentialSet(ctx context.Context, ref, value string) error {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
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
	if err := a.store.SetSetting(ctx, "llm.key."+provider, value); err != nil {
		return err
	}
	if a.llmKeys == nil {
		a.llmKeys = make(map[string]string)
	}
	a.llmKeys[provider] = value
	if a.llmReg != nil {
		if err := a.registerLLM(); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) nativeCredentialUnset(ctx context.Context, ref string) error {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	if a.store == nil {
		return errors.New("credential store is not available")
	}
	provider, err := a.nativeCredentialProvider(ref)
	if err != nil {
		return err
	}
	if err := a.store.DeleteSetting(ctx, "llm.key."+provider); err != nil {
		return err
	}
	delete(a.llmKeys, provider)
	if a.llmReg != nil {
		if err := a.registerLLM(); err != nil {
			return err
		}
	}
	return nil
}
