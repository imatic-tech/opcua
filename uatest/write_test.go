//go:build integration
// +build integration

package uatest

import (
	"context"
	"testing"

	"github.com/imatic-tech/opcua"
	"github.com/imatic-tech/opcua/ua"
)

// TestWrite performs an integration test to first write
// and then read values from an OPC/UA server.
func TestWrite(t *testing.T) {
	tests := []struct {
		id     *ua.NodeID
		v      interface{}
		status ua.StatusCode
	}{
		// happy flows
		{ua.NewStringNodeID(2, "rw_bool"), false, ua.StatusOK},
		{ua.NewStringNodeID(2, "rw_int32"), int32(9), ua.StatusOK},

		// error flows
		{ua.NewStringNodeID(2, "ro_bool"), false, ua.StatusBadUserAccessDenied},
	}

	ctx := context.Background()

	srv := NewServer("rw_server.py")
	defer srv.Close()

	c := opcua.NewClient(srv.Endpoint, srv.Opts...)
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.CloseWithContext(ctx)

	for _, tt := range tests {
		t.Run(tt.id.String(), func(t *testing.T) {
			testWrite(t, ctx, c, tt.status, &ua.WriteRequest{
				NodesToWrite: []*ua.WriteValue{
					&ua.WriteValue{
						NodeID:      tt.id,
						AttributeID: ua.AttributeIDValue,
						Value: &ua.DataValue{
							EncodingMask: ua.DataValueValue,
							Value:        ua.MustVariant(tt.v),
						},
					},
				},
			})

			// skip read tests if the write is expected to fail
			if tt.status != ua.StatusOK {
				return
			}

			testRead(t, ctx, c, tt.v, tt.id)
		})
	}
}

func testWrite(t *testing.T, ctx context.Context, c *opcua.Client, status ua.StatusCode, req *ua.WriteRequest) {
	t.Helper()

	resp, err := c.WriteWithContext(ctx, req)
	if err != nil {
		t.Fatalf("Write failed: %s", err)
	}
	if got, want := resp.Results[0], status; got != want {
		t.Fatalf("got status %v want %v", got, want)
	}
}
