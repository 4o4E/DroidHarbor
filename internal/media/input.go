package media

import (
	"context"
	"errors"
	"math"

	"droidharbor/internal/scrcpy"
)

// Input ownership is local to one shared SDK controller. Releasing a window must
// not lift another window's key or pointer. It cannot undo external ADB actions.
type inputState struct {
	owners      map[string]*ownerInputs
	keys        map[int32]heldKey
	pointers    int
	nextPointer int64
}

type heldKey struct {
	count int
	down  scrcpy.ControlMessage
}

type ownerInputs struct {
	keys     map[int32]struct{}
	pointers map[int64]scrcpy.ControlMessage
}

type controlSender func(context.Context, scrcpy.ControlMessage, func() error) error

func (s *inputState) owner(id string) *ownerInputs {
	if s.owners == nil {
		s.owners = make(map[string]*ownerInputs)
		s.keys = make(map[int32]heldKey)
	}
	state := s.owners[id]
	if state == nil {
		state = &ownerInputs{keys: make(map[int32]struct{}), pointers: make(map[int64]scrcpy.ControlMessage)}
		s.owners[id] = state
	}
	return state
}

// Caller serializes all methods with the device input mutex, including Send.
func (s *inputState) key(ctx context.Context, owner string, msg scrcpy.ControlMessage, send controlSender, authorize func() error) error {
	if msg.Action > 1 || msg.Keycode < 0 || msg.Repeat < 0 || msg.MetaState < 0 {
		return problem(422, "INVALID_INPUT", "按键参数无效")
	}
	state := s.owner(owner)
	_, held := state.keys[msg.Keycode]
	global := s.keys[msg.Keycode]
	if msg.Action == 0 {
		if !held && msg.Repeat != 0 {
			return problem(409, "INPUT_NOT_HELD", "重复按键必须属于当前会话")
		}
		if held || global.count == 0 {
			if err := send(ctx, msg, authorize); err != nil {
				return err
			}
		} else if err := authorize(); err != nil {
			return err
		}
		if !held {
			if global.count == 0 {
				global.down = msg
			}
			global.count++
			s.keys[msg.Keycode] = global
			state.keys[msg.Keycode] = struct{}{}
		}
		return nil
	}
	if !held {
		return problem(409, "INPUT_NOT_HELD", "当前会话没有按住该按键")
	}
	if global.count == 1 {
		prior := global.down
		prior.Action, prior.Repeat, prior.MetaState = 1, 0, msg.MetaState
		if err := send(ctx, prior, authorize); err != nil {
			return err
		}
	} else if err := authorize(); err != nil {
		return err
	}
	s.dropKey(msg.Keycode)
	delete(state.keys, msg.Keycode)
	return nil
}

func (s *inputState) touch(ctx context.Context, owner string, localPointer int64, msg scrcpy.ControlMessage, send controlSender, authorize func() error) error {
	if msg.Action > 2 || math.IsNaN(float64(msg.Pressure)) || msg.Pressure < 0 || msg.Pressure > 1 {
		return problem(422, "INVALID_INPUT", "触控参数无效")
	}
	state := s.owner(owner)
	prior, held := state.pointers[localPointer]
	if msg.Action == 0 {
		if held {
			return problem(409, "INPUT_ALREADY_HELD", "该指针已经按下")
		}
		if s.pointers == 10 || s.nextPointer == math.MaxInt64 {
			return problem(409, "INPUT_LIMIT", "SDK 控制器的同时触控数量已达到上限")
		}
		if msg.Position.X < 0 || msg.Position.Y < 0 || msg.Position.X >= int32(msg.Position.Width) || msg.Position.Y >= int32(msg.Position.Height) {
			return problem(422, "INVALID_POSITION", "按下位置不在手机画面内")
		}
		s.nextPointer++
		msg.PointerID = s.nextPointer
	} else {
		if !held {
			return problem(409, "INPUT_NOT_HELD", "当前会话没有按住该指针")
		}
		msg.PointerID = prior.PointerID
	}
	// Default SDK primary clicks are fingers. Secondary buttons are explicit
	// back/home/app-switch commands, not a shared POINTER_ID_MOUSE gesture.
	msg.ActionButton, msg.Buttons = 0, 0
	if err := send(ctx, msg, authorize); err != nil {
		return err
	}
	if msg.Action == 1 {
		delete(state.pointers, localPointer)
		s.pointers--
	} else {
		if !held {
			s.pointers++
		}
		state.pointers[localPointer] = msg
	}
	return nil
}

func (s *inputState) release(ctx context.Context, owner string, width, height uint16, send controlSender, authorize func() error) error {
	state := s.owners[owner]
	if state == nil {
		return nil
	}
	var result error
	for key := range state.keys {
		if global := s.keys[key]; global.count == 1 {
			prior := global.down
			prior.Action, prior.Repeat, prior.MetaState = 1, 0, 0
			result = errors.Join(result, send(ctx, prior, authorize))
		}
		s.dropKey(key)
	}
	for _, prior := range state.pointers {
		prior.Action, prior.Pressure = 1, 0
		// A rotation invalidates the old size in scrcpy's position mapper. An UP
		// must use the current size; it never starts a replacement gesture.
		if width != 0 && height != 0 {
			prior.Position.X = int32(int64(prior.Position.X) * int64(width) / int64(prior.Position.Width))
			prior.Position.Y = int32(int64(prior.Position.Y) * int64(height) / int64(prior.Position.Height))
			prior.Position.Width, prior.Position.Height = width, height
		}
		result = errors.Join(result, send(ctx, prior, authorize))
		s.pointers--
	}
	delete(s.owners, owner)
	return result
}

func (s *inputState) releaseAll(ctx context.Context, width, height uint16, send controlSender, authorize func() error) error {
	var result error
	for owner := range s.owners {
		result = errors.Join(result, s.release(ctx, owner, width, height, send, authorize))
	}
	return result
}

func (s *inputState) dropKey(key int32) {
	global := s.keys[key]
	global.count--
	if global.count == 0 {
		delete(s.keys, key)
	} else {
		s.keys[key] = global
	}
}

func (s *inputState) discard(owner string) {
	state := s.owners[owner]
	if state == nil {
		return
	}
	for key := range state.keys {
		s.dropKey(key)
	}
	s.pointers -= len(state.pointers)
	delete(s.owners, owner)
}
