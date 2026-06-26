package main

import "testing"

func TestFilterUpgradeTargets(t *testing.T) {
	targets := []GatewayTarget{
		{Cluster: "doops-jm", Instance: "jm-228"},
		{Cluster: "doops-89", Instance: "master-node"},
		{Cluster: "doops-89", Instance: "worker"},
	}
	got := filterUpgradeTargets(targets, "doops-89", "*")
	if len(got) != 2 {
		t.Fatalf("expected two doops-89 targets, got %#v", got)
	}
	got = filterUpgradeTargets(targets, "*", "jm-228")
	if len(got) != 1 || got[0].Cluster != "doops-jm" {
		t.Fatalf("expected jm target, got %#v", got)
	}
	got = filterUpgradeTargets(targets, "missing", "*")
	if len(got) != 0 {
		t.Fatalf("expected no targets, got %#v", got)
	}
}
