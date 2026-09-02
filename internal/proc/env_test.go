package proc

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSpec_Environ(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("SC_ENV_A", "a")
	t.Setenv("SC_ENV_B", "b")
	t.Setenv("SC_ENV_SECRET", "s")

	t.Run("only PATH by default", func(t *testing.T) {
		assert.Equal(t, []string{"PATH=/usr/bin:/bin"}, Spec{}.Environ())
	})

	t.Run("inherit and explicit, sorted, explicit wins", func(t *testing.T) {
		spec := Spec{
			InheritEnv: []string{"SC_ENV_B", "SC_ENV_A", "SC_ENV_MISSING"},
			Env:        map[string]string{"SC_ENV_B": "override", "SC_ENV_Z": "z", "EMPTY": ""},
		}
		assert.Equal(t, []string{
			"EMPTY=",
			"PATH=/usr/bin:/bin",
			"SC_ENV_A=a",
			"SC_ENV_B=override",
			"SC_ENV_Z=z",
		}, spec.Environ())
	})

	t.Run("PATH can be overridden explicitly", func(t *testing.T) {
		spec := Spec{Env: map[string]string{"PATH": "/opt/shims:/usr/bin"}}
		assert.Equal(t, []string{"PATH=/opt/shims:/usr/bin"}, spec.Environ())
	})
}

func TestEnvValue(t *testing.T) {
	t.Parallel()
	env := []string{"A=1", "AB=2", "C=x=y"}
	v, ok := EnvValue(env, "A")
	assert.True(t, ok)
	assert.Equal(t, "1", v)
	v, ok = EnvValue(env, "C")
	assert.True(t, ok)
	assert.Equal(t, "x=y", v)
	_, ok = EnvValue(env, "B")
	assert.False(t, ok)
}
