/*
Copyright 2026 Steward Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package provider

import (
	"errors"
	"testing"
)

func TestFactory_UnsetProviderFailsClosed(t *testing.T) {
	t.Setenv("STEWARD_PROVIDER", "")

	p, err := NewFactory().GetProvider()
	if !errors.Is(err, ErrProviderNotSet) {
		t.Fatalf("GetProvider() error = %v, want ErrProviderNotSet", err)
	}
	if p != nil {
		t.Fatalf("GetProvider() returned %s; an unset provider must not fall back to anything", p.Name())
	}
}

func TestFactory_MockMustBeChosenExplicitly(t *testing.T) {
	t.Setenv("STEWARD_PROVIDER", "mock")

	p, err := NewFactory().GetProvider()
	if err != nil {
		t.Fatalf("GetProvider() error = %v", err)
	}
	if p.Name() != "mock" {
		t.Fatalf("provider = %s, want mock", p.Name())
	}
}

func TestFactory_UnknownProviderIsRejected(t *testing.T) {
	t.Setenv("STEWARD_PROVIDER", "kubernets")

	if _, err := NewFactory().GetProvider(); err == nil {
		t.Fatal("GetProvider() accepted a misspelled provider")
	}
}
