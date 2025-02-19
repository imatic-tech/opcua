// Copyright 2018-2020 opcua authors. All rights reserved.
// Use of this source code is governed by a MIT-style license that can be
// found in the LICENSE file.

package main

import (
	"context"
	"flag"
	"log"

	"github.com/imatic-tech/opcua"
	"github.com/imatic-tech/opcua/debug"
	"github.com/imatic-tech/opcua/ua"
)

func main() {
	var (
		endpoint = flag.String("endpoint", "opc.tcp://localhost:4840", "OPC UA Endpoint URL")
		nodeID   = flag.String("node", "", "NodeID to read")
	)
	flag.BoolVar(&debug.Enable, "debug", false, "enable debug logging")
	flag.Parse()
	log.SetFlags(0)

	ctx := context.Background()

	c := opcua.NewClient(*endpoint)
	if err := c.Connect(ctx); err != nil {
		log.Fatal(err)
	}
	defer c.CloseWithContext(ctx)

	id, err := ua.ParseNodeID(*nodeID)
	if err != nil {
		log.Fatal(err)
	}

	n := c.Node(id)
	accessLevel, err := n.AccessLevelWithContext(ctx)
	if err != nil {
		log.Fatal(err)
	}
	log.Print("AccessLevel: ", accessLevel)

	userAccessLevel, err := n.UserAccessLevelWithContext(ctx)
	if err != nil {
		log.Fatal(err)
	}
	log.Print("UserAccessLevel: ", userAccessLevel)

	v, err := n.ValueWithContext(ctx)
	switch {
	case err != nil:
		log.Fatal(err)
	case v == nil:
		log.Print("v == nil")
	default:
		log.Print(v.Value())
	}
}
