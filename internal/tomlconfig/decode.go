// Package tomlconfig decodes configuration without silently dropping unknown keys.
package tomlconfig

import (
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

func DecodeFile(path string, target any) (toml.MetaData, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return toml.MetaData{}, err
	}
	return Decode(string(data), target)
}

func Decode(data string, target any) (toml.MetaData, error) {
	metadata, err := toml.Decode(data, target)
	if err != nil {
		return metadata, err
	}
	unknown := metadata.Undecoded()
	if len(unknown) != 0 {
		keys := make([]string, len(unknown))
		for i, key := range unknown {
			keys[i] = key.String()
		}
		return metadata, fmt.Errorf("unknown configuration keys: %s", strings.Join(keys, ", "))
	}
	return metadata, nil
}
