package main

import "github.com/Nezhinskiy/coreslot/internal/config"

// windowsConfig is the configuration a Windows run reads for [platform] windows_warning: the
// one config.Path names, which is <statedir>/config.toml under CORESLOT_STATE_DIR. A
// configuration that cannot be read leaves the warning on.
func windowsConfig() config.Config {
	path, err := config.Path()
	if err != nil {
		return config.Config{WindowsWarning: true}
	}
	cfg, err := config.Load(path)
	if err != nil {
		return config.Config{WindowsWarning: true}
	}
	return cfg
}
