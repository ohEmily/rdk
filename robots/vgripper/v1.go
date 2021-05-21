// Package vgripper implements versions of the Viam gripper.
package vgripper

import (
	"context"
	"math"
	"time"

	"github.com/go-errors/errors"

	"go.viam.com/core/board"
	"go.viam.com/core/config"
	"go.viam.com/core/gripper"
	pb "go.viam.com/core/proto/api/v1"
	"go.viam.com/core/registry"
	"go.viam.com/core/robot"
	"go.viam.com/core/utils"

	"github.com/edaniels/golog"
	"go.uber.org/multierr"
)

func init() {
	registry.RegisterGripper("viam", func(ctx context.Context, r robot.Robot, config config.Component, logger golog.Logger) (gripper.Gripper, error) {
		b := r.BoardByName("local")
		if b == nil {
			return nil, errors.New("viam gripper requires a board called local")
		}
		return NewGripperV1(ctx, b, config.Attributes.Int("pressureLimit", 800), logger)
	})
}

// TODO
const (
	MaxCurrent              = 300
	CurrentBadReadingCounts = 15
	MinRotationGap          = 2.0
	MaxRotationGap          = 3.0
)

// GripperV1 TODO
type GripperV1 struct {
	motor    board.Motor
	current  board.AnalogReader
	pressure board.AnalogReader

	openPos, closePos float64

	defaultPowerPct, holdingPressure float32

	pressureLimit int

	closeDirection, openDirection pb.DirectionRelative
	logger                        golog.Logger

	numBadCurrentReadings int
}

// NewGripperV1 TODO
func NewGripperV1(ctx context.Context, theBoard board.Board, pressureLimit int, logger golog.Logger) (*GripperV1, error) {

	vg := &GripperV1{
		motor:           theBoard.Motor("g"),
		current:         theBoard.AnalogReader("current"),
		pressure:        theBoard.AnalogReader("pressure"),
		defaultPowerPct: 1.0,
		holdingPressure: .5,
		pressureLimit:   pressureLimit,
		logger:          logger,
	}

	if vg.motor == nil {
		return nil, errors.New("gripper needs a motor named 'g'")
	}
	supported, err := vg.motor.PositionSupported(ctx)
	if err != nil {
		return nil, err
	}
	if !supported {
		return nil, errors.New("gripper motor needs to support position")
	}

	if vg.current == nil || vg.pressure == nil {
		return nil, errors.New("gripper needs a current and a pressure reader")
	}

	// pick a direction and move till it stops
	posA, hasPressureA, err := vg.moveInDirectionTillWontMoveMore(ctx, pb.DirectionRelative_DIRECTION_RELATIVE_FORWARD)
	if err != nil {
		return nil, err
	}

	posB, hasPressureB, err := vg.moveInDirectionTillWontMoveMore(ctx, pb.DirectionRelative_DIRECTION_RELATIVE_BACKWARD)
	if err != nil {
		return nil, err
	}

	if hasPressureA == hasPressureB {
		return nil, errors.Errorf("pressure same open and closed, something is wrong encoer: %f %f", posA, posB)
	}

	if hasPressureA {
		vg.closeDirection = pb.DirectionRelative_DIRECTION_RELATIVE_FORWARD
		vg.openDirection = pb.DirectionRelative_DIRECTION_RELATIVE_BACKWARD
		vg.openPos = posB
		vg.closePos = posA
	} else {
		vg.closeDirection = pb.DirectionRelative_DIRECTION_RELATIVE_BACKWARD
		vg.openDirection = pb.DirectionRelative_DIRECTION_RELATIVE_FORWARD
		vg.openPos = posA
		vg.closePos = posB
	}

	rotationGap := math.Abs(vg.openPos - vg.closePos)
	if rotationGap < MinRotationGap || rotationGap > MaxRotationGap {
		return nil, errors.Errorf("rotationGap not in expected range got: %v range %v -> %v", rotationGap, MinRotationGap, MaxRotationGap)
	}

	return vg, vg.Open(ctx)
}

// Open TODO
func (vg *GripperV1) Open(ctx context.Context) error {
	err := vg.motor.Go(ctx, vg.openDirection, vg.defaultPowerPct)
	if err != nil {
		return err
	}

	msPer := 10
	total := 0
	for {
		if !utils.SelectContextOrWait(ctx, time.Duration(msPer)*time.Millisecond) {
			return vg.stopAfterError(ctx, ctx.Err())
		}
		now, err := vg.motor.Position(ctx)
		if err != nil {
			return vg.stopAfterError(ctx, err)
		}
		if vg.encoderSame(now, vg.openPos) {
			return vg.Stop(ctx)
		}

		current, err := vg.readCurrent(ctx)
		if err != nil {
			return vg.stopAfterError(ctx, err)
		}
		err = vg.processCurrentReading(ctx, current, "opening")
		if err != nil {
			return vg.stopAfterError(ctx, err)
		}

		total += msPer
		if total > 5000 {
			return vg.stopAfterError(ctx, errors.Errorf("open timed out, wanted: %f at: %f", vg.openPos, now))
		}
	}
}

// Grab TODO
func (vg *GripperV1) Grab(ctx context.Context) (bool, error) {
	err := vg.motor.Go(ctx, vg.closeDirection, vg.defaultPowerPct)
	if err != nil {
		return false, err
	}

	msPer := 10
	total := 0
	for {
		if !utils.SelectContextOrWait(ctx, time.Duration(msPer)*time.Millisecond) {
			return false, vg.stopAfterError(ctx, ctx.Err())
		}
		now, err := vg.motor.Position(ctx)
		if err != nil {
			return false, vg.stopAfterError(ctx, err)
		}

		if vg.encoderSame(now, vg.closePos) {
			// we are fully closed
			return false, vg.Stop(ctx)
		}

		pressure, _, current, err := vg.analogs(ctx)
		if err != nil {
			return false, vg.stopAfterError(ctx, err)
		}
		err = vg.processCurrentReading(ctx, current, "grabbing")
		if err != nil {
			return false, vg.stopAfterError(ctx, err)
		}

		if pressure {
			vg.logger.Debugf("i think i grabbed something, have pressure, pos: %f closePos: %v", now, vg.closePos)
			err := vg.motor.Go(ctx, vg.closeDirection, vg.holdingPressure)
			return true, err
		}

		total += msPer
		if total > 5000 {
			pressureRaw, err := vg.readPressure(ctx)
			if err != nil {
				return false, err
			}
			return false, vg.stopAfterError(ctx, errors.Errorf("close timed out, wanted: %f at: %f pressure: %d", vg.closePos, now, pressureRaw))
		}
	}
}

func (vg *GripperV1) processCurrentReading(ctx context.Context, current int, where string) error {
	if current < MaxCurrent {
		vg.numBadCurrentReadings = 0
		return nil
	}
	vg.numBadCurrentReadings++
	if vg.numBadCurrentReadings < CurrentBadReadingCounts {
		return nil
	}
	return errors.Errorf("current too high for too long, currently %d during %s", current, where)
}

// Close TODO
func (vg *GripperV1) Close() error {
	return vg.Stop(context.Background())
}

func (vg *GripperV1) stopAfterError(ctx context.Context, other error) error {
	return multierr.Combine(other, vg.motor.Off(ctx))
}

// Stop TODO
func (vg *GripperV1) Stop(ctx context.Context) error {
	return vg.motor.Off(ctx)
}

func (vg *GripperV1) readCurrent(ctx context.Context) (int, error) {
	return vg.current.Read(ctx)
}

func (vg *GripperV1) encoderSame(a, b float64) bool {
	return math.Abs(b-a) < .1
}

func (vg *GripperV1) readPressure(ctx context.Context) (int, error) {
	return vg.pressure.Read(ctx)
}

func (vg *GripperV1) hasPressure(ctx context.Context) (bool, int, error) {
	p, err := vg.readPressure(ctx)
	return p < vg.pressureLimit, p, err
}

func (vg *GripperV1) analogs(ctx context.Context) (hasPressure bool, pressure, current int, err error) {
	hasPressure, pressure, err = vg.hasPressure(ctx)
	if err != nil {
		return
	}

	current, err = vg.readCurrent(ctx)
	if err != nil {
		return
	}

	return
}

func (vg *GripperV1) moveInDirectionTillWontMoveMore(ctx context.Context, dir pb.DirectionRelative) (float64, bool, error) {
	defer func() {
		err := vg.Stop(ctx)
		if err != nil {
			vg.logger.Warnf("couldn't stop motor %s", err)
		}
		vg.logger.Debug("stopped")
	}()

	vg.logger.Debugf("starting to move dir: %v", dir)

	err := vg.motor.Go(ctx, dir, vg.defaultPowerPct)
	if err != nil {
		return -1, false, err
	}

	last, err := vg.motor.Position(ctx)
	if err != nil {
		return -1, false, err
	}

	if !utils.SelectContextOrWait(ctx, 500*time.Millisecond) {
		return -1, false, ctx.Err()
	}

	for {
		now, err := vg.motor.Position(ctx)
		if err != nil {
			return -1, false, err
		}

		hasPressure, pressure, current, err := vg.analogs(ctx)
		if err != nil {
			return -1, false, err
		}

		vg.logger.Debugf("dir: %v last: %v now: %v hasPressure: %v pressure: %v",
			dir, last, now, hasPressure, pressure)

		if vg.encoderSame(last, now) || hasPressure {
			// increase power temporarily
			err := vg.motor.Power(ctx, vg.defaultPowerPct*2)
			if err != nil {
				return -1, false, err
			}
			if !utils.SelectContextOrWait(ctx, 200*time.Millisecond) {
				return -1, false, ctx.Err()
			}

			hasPressure, pressure, _, err := vg.analogs(ctx)
			if err != nil {
				return -1, false, err
			}

			vg.logger.Debugf("inner dir: %v last: %v now: %v hasPressure: %v pressure: %v",
				dir, last, now, hasPressure, pressure)

			return now, hasPressure, err
		}
		last = now

		err = vg.processCurrentReading(ctx, current, "init")
		if err != nil {
			return -1, false, err
		}

		if !utils.SelectContextOrWait(ctx, 100*time.Millisecond) {
			return -1, false, ctx.Err()
		}
	}

}
