package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadProxyHealthProbeIntervalMinutes(t *testing.T) {
	t.Run("defaults to ten minutes", func(t *testing.T) {
		resetViperWithJWTSecret(t)
		cfg, err := Load()
		require.NoError(t, err)
		require.Equal(t, 10, cfg.ProxyHealthProbeIntervalMinutes)
	})

	for _, test := range []struct {
		name  string
		value string
		want  int
	}{
		{name: "custom positive", value: "7", want: 7},
		{name: "zero disables", value: "0", want: 0},
		{name: "negative disables", value: "-2", want: -2},
	} {
		t.Run(test.name, func(t *testing.T) {
			resetViperWithJWTSecret(t)
			t.Setenv("PROXY_HEALTH_PROBE_INTERVAL_MINUTES", test.value)
			cfg, err := Load()
			require.NoError(t, err)
			require.Equal(t, test.want, cfg.ProxyHealthProbeIntervalMinutes)
		})
	}
}
