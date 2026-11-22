// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package linktrain

import "errors"

// TrainErrorCode represents the enumerated training failure reasons.
type TrainErrorCode int

const (
	// ErrNone indicates no training error.
	ErrNone TrainErrorCode = iota
	// ErrLinkUpTimeout indicates a timeout occurred while waiting for the link to come up.
	ErrLinkUpTimeout
	// ErrSecPCIeExtCapNotFound indicates the Secondary PCIe Extended Capability was not found.
	ErrSecPCIeExtCapNotFound
	// ErrSetSBRFailed indicates unable to set the SBR bit during secondary bus reset.
	ErrSetSBRFailed
	// ErrDisableTimeout indicates a timeout occurred while disabling the link.
	ErrDisableTimeout
	// ErrLinkDown indicates the link is down (USP register value is 0xffff).
	ErrLinkDown
	// ErrDspUspSpeedMismatch indicates the DSP and USP link speeds are not the same.
	ErrDspUspSpeedMismatch
	// ErrPCIeCapNotFound indicates the PCIe Capability was not found in the capability linked list.
	ErrPCIeCapNotFound
	// ErrCapChainUnaligned indicates the capability linked list has an unaligned pointer.
	ErrCapChainUnaligned
	// ErrCapChainLoop indicates the capability linked list loops back on itself.
	ErrCapChainLoop
	// ErrInvalidSpeedStep indicates M_SPEED_STEP steps that cannot be trained: no step is in the
	// Supported Link Speeds Vector of both ends of the link, or a step does not change the link
	// speed, so its retrain would not be observable.
	ErrInvalidSpeedStep
	// ErrNoSupportedLinkSpeeds indicates a port whose Link Capabilities 2 Register has no Supported
	// Link Speeds Vector, so M_SPEED_STEP cannot tell which speeds it supports.
	ErrNoSupportedLinkSpeeds
)

func (c TrainErrorCode) String() string {
	switch c {
	case ErrNone:
		return "no error"
	case ErrLinkUpTimeout:
		return "link training timeout"
	case ErrSecPCIeExtCapNotFound:
		return "secondary PCIe extended cap not found"
	case ErrSetSBRFailed:
		return "failed to set SBR bit"
	case ErrDisableTimeout:
		return "link disable timeout"
	case ErrLinkDown:
		return "link down (USP register value is 0xffff)"
	case ErrDspUspSpeedMismatch:
		return "DSP and USP link speeds are not the same"
	case ErrPCIeCapNotFound:
		return "PCIe capability not found"
	case ErrCapChainUnaligned:
		return "capability chain has an unaligned pointer"
	case ErrCapChainLoop:
		return "capability chain loops"
	case ErrInvalidSpeedStep:
		return "invalid speed step"
	case ErrNoSupportedLinkSpeeds:
		return "no supported link speeds vector"
	default:
		return "unknown training error"
	}
}

// TrainError encapsulates the training error code with a detailed message.
type TrainError struct {
	Code    TrainErrorCode
	Message string
}

func (e *TrainError) Error() string {
	if e == nil {
		return "no error"
	}
	if e.Message != "" {
		return e.Code.String() + ": " + e.Message
	}
	return e.Code.String()
}

// Is reports whether target is a *TrainError with the same Code, so that errors.Is finds a
// TrainError by its Code anywhere in an error tree, including in joined errors. A nil *TrainError
// matches nothing.
func (e *TrainError) Is(target error) bool {
	t, ok := target.(*TrainError)
	return ok && e != nil && t != nil && e.Code == t.Code
}

// needFieldCheck returns true if the error indicates a training timeout (ErrLinkUpTimeout)
// which still requires running the speed check.
//
// It returns false if any error in err's tree indicates the link is down, e.g. a speed step
// timeout joined with a link down found while restoring the speed, because a field read against a
// device that is not responding is not a measurement.
func needFieldCheck(err error) bool {
	if err == nil || isLinkDown(err) {
		return false
	}
	var te *TrainError
	if errors.As(err, &te) && te != nil {
		switch te.Code {
		case ErrLinkUpTimeout:
			return true
		case ErrNone, ErrSecPCIeExtCapNotFound, ErrSetSBRFailed, ErrDisableTimeout, ErrLinkDown, ErrDspUspSpeedMismatch,
			ErrPCIeCapNotFound, ErrCapChainUnaligned, ErrCapChainLoop, ErrInvalidSpeedStep, ErrNoSupportedLinkSpeeds:
			return false
		}
	}
	return false
}

// isLinkDown reports whether any error in err's tree indicates the device stopped responding to
// config space access, i.e. every read came back as all ones. Such an iteration says nothing about
// link speed or width, so the train loop reports it separately from a genuine field mismatch.
//
// errors.Is checks every error in the tree, so ErrLinkDown is found even when another TrainError
// is joined before it.
func isLinkDown(err error) bool {
	return errors.Is(err, &TrainError{Code: ErrLinkDown})
}
