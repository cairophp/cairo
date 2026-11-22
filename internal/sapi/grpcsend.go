package sapi

/*
#include "grpc_stream.h"
*/
import "C"

import (
	"unsafe"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cairophp/cairo/internal/grpcserver"
)

func init() {
	registerC(&C.cairo_grpc_stream)
}

// goGrpcSend queues one reply of the stream PHP is serving. It runs on the
// PHP thread and never waits: 1 when the reply is queued, 0 when the queue
// is full, or the status code with its text in *errOut (the caller frees
// it). data is PHP's memory, read here and not kept.
//
//export goGrpcSend
func goGrpcSend(stream C.longlong, data *C.char, n C.int, errOut **C.char) C.int {
	reply := unsafe.Slice((*byte)(unsafe.Pointer(data)), int(n))
	queued, err := grpcserver.StreamTrySend(int64(stream), reply)
	return sendOutcome(queued, err, errOut)
}

// goGrpcClientSend is goGrpcSend for one request of a stream that PHP
// opened as a client.
//
//export goGrpcClientSend
func goGrpcClientSend(stream C.longlong, data *C.char, n C.int, errOut **C.char) C.int {
	v, ok := clientStreams.Load(int64(stream))
	if !ok {
		*errOut = C.CString("the stream is closed")
		return C.int(codes.Canceled)
	}
	request := unsafe.Slice((*byte)(unsafe.Pointer(data)), int(n))
	queued, err := v.(*clientStream).trySend(request)
	return sendOutcome(queued, err, errOut)
}

func sendOutcome(queued bool, err error, errOut **C.char) C.int {
	if err != nil {
		st := status.Convert(err)
		*errOut = C.CString(st.Message())
		return C.int(st.Code())
	}
	if queued {
		return 1
	}
	return 0
}
