/*
Copyright 2022 The Kubernetes-CSI-Addons Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package util

import (
	"fmt"
	"runtime"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GetErrorMessage returns the message from the error if it is a grpc error,
// else returns err.Error().
func GetErrorMessage(err error) string {
	s, ok := status.FromError(err)
	if !ok {
		return err.Error()
	}

	return s.Message()
}

// IsUnimplementedError returns true if the error is Unimplemented error.
func IsUnimplementedError(err error) bool {
	s, ok := status.FromError(err)
	if !ok {
		return false
	}

	return s.Code() == codes.Unimplemented
}

// IsAbortedError returns true if the error is a gRPC Aborted error.
func IsAbortedError(err error) bool {
	if err == nil {
		return false
	}

	s, ok := status.FromError(err)
	if !ok {
		return false
	}

	return s.Code() == codes.Aborted
}

// IsOutOfRangeError returns true if the error is a gRPC OutOfRange error.
func IsOutOfRangeError(err error) bool {
	s, ok := status.FromError(err)
	if !ok {
		return false
	}

	return s.Code() == codes.OutOfRange
}

// skip = 0 means the caller of this function, skip = 1 means the caller of the caller, and so on
func CallerInfo(skip int) string {
	pc, file, line, ok := runtime.Caller(skip + 1)
	if !ok {
		return "unknown"
	}
	return fmt.Sprintf("%s:%d:%s", file, line, runtime.FuncForPC(pc).Name())
}
