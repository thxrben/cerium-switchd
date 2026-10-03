package dataplane

import "github.com/thxrben/cerium-switchd/pkg/netdev"

// FlushLearned removes the MAC addresses the bridge learned on dev
// (netdev.FlushLearned).
func FlushLearned(dev string) (int, error) { return netdev.FlushLearned(dev) }
