// Copyright 2018-2020 opcua authors. All rights reserved.
// Use of this source code is governed by a MIT-style license that can be
// found in the LICENSE file.

package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"github.com/imatic-tech/opcua"
	"github.com/imatic-tech/opcua/debug"
	"github.com/imatic-tech/opcua/id"
	"github.com/imatic-tech/opcua/ua"
)

func main() {
	endpoint := flag.String("endpoint", "opc.tcp://localhost:4840", "OPC UA Endpoint URL")
	nodePath := flag.String("path", "device_led.temperature", "path of a node's browse name")
	ns := flag.Int("namespace", 0, "namespace of the node")
	flag.BoolVar(&debug.Enable, "debug", false, "enable debug logging")

	flag.Parse()
	log.SetFlags(0)

	ctx := context.Background()

	c := opcua.NewClient(*endpoint)
	if err := c.Connect(ctx); err != nil {
		log.Fatal(err)
	}
	defer c.CloseWithContext(ctx)

	root := c.Node(ua.NewTwoByteNodeID(id.ObjectsFolder))
	nodeID, err := root.TranslateBrowsePathInNamespaceToNodeIDWithContext(ctx, uint16(*ns), *nodePath)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(nodeID)
}
