// Copyright (C) 2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package rpc

import (
	"context"
	"net/http"

	"github.com/ava-labs/avalanchego/trace"
)

type StateReader interface {
	Tracer() trace.Tracer
	ReadState(ctx context.Context, keys [][]byte) ([][]byte, []error)
}

type ReadStateRequest struct {
	Keys [][]byte
}

type ReadStateResponse struct {
	Values [][]byte
	Errors []error
}

func NewJSONRPCStateServer(stateReader StateReader) *JSONRPCStateServer {
	return &JSONRPCStateServer{
		stateReader: stateReader,
	}
}

// JSONRPCStateServer gives direct read access to the vm state
type JSONRPCStateServer struct {
	stateReader StateReader
}

func (s *JSONRPCStateServer) ReadState(req *http.Request, args *ReadStateRequest, res *ReadStateResponse) error {
	ctx, span := s.stateReader.Tracer().Start(req.Context(), "Server.ReadState")
	defer span.End()

	res.Values, res.Errors = s.stateReader.ReadState(ctx, args.Keys)
	return nil
}