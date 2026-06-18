package main

import (
	"net"
	"reflect"
	"testing"
)

func TestSortedPeers(t *testing.T) {
	records := []*net.SRV{
		{Target: "db2.addr.example.COnSul.", Port: 4567, Priority: 10, Weight: 10},
		{Target: "db1.addr.example.consul.", Port: 4567, Priority: 10, Weight: 20},
		{Target: "db2.addr.example.consul.", Port: 4567, Priority: 10, Weight: 10},
		{Target: "db3.addr.example.consul.", Port: 4567, Priority: 20, Weight: 10},
	}
	got := sortedPeers(records)
	want := []string{
		"db1.addr.example.consul:4567",
		"db2.addr.example.consul:4567",
		"db3.addr.example.consul:4567",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sortedPeers() = %#v, want %#v", got, want)
	}
}
