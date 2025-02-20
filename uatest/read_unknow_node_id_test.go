//go:build integration
// +build integration

package uatest

import (
	"context"
	"testing"

	"github.com/imatic-tech/opcua"
	"github.com/imatic-tech/opcua/id"
	"github.com/imatic-tech/opcua/ua"
)

// TestRead performs an integration test to read values
// from an OPC/UA server.
func TestReadUnknowNodeID(t *testing.T) {
	ctx := context.Background()

	srv := NewServer("read_unknow_node_id_server.py")
	defer srv.Close()

	c := opcua.NewClient(srv.Endpoint, srv.Opts...)
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.CloseWithContext(ctx)

	// read node with unknown extension object
	// This should be OK
	nodeWithUnknownType := ua.NewStringNodeID(2, "IntValZero")
	resp, err := c.Read(&ua.ReadRequest{
		NodesToRead: []*ua.ReadValueID{
			{NodeID: nodeWithUnknownType},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if got, want := resp.Results[0].Status, ua.StatusBadDataTypeIDUnknown; got != want {
		t.Errorf("got status %v want %v for a node with an unknown type", got, want)
	}

	// check that the connection is still usable by reading another node.
	_, err = c.ReadWithContext(ctx, &ua.ReadRequest{
		NodesToRead: []*ua.ReadValueID{
			{
				NodeID: ua.NewNumericNodeID(0, id.Server_ServerStatus_State),
			},
		},
	})
	if err != nil {
		t.Error(err)
	}
}
