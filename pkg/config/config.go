// Package config configures the xDS servers.
package config

import (
	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
)

type ClusterConfig struct {
	NameTemplate string                `json:"name_template"`
	BaseConfig   *envoy_api_v2.Cluster `json:"base"`
}

type Config struct {
	ClusterConfig *ClusterConfig `json:"cluster_config"`
}
