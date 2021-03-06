package gopisysfs

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type GPIOFlag struct {
	flag bool
	err  error
}

type GPIOMode int

const (
	GPIOInput GPIOMode = iota
	GPIOOutput
	GPIOOutputLow
	GPIOOutputHigh

	// from https://www.kernel.org/doc/Documentation/gpio/sysfs.txt
	direction_in     = "in"
	direction_out    = "out"
	direction_outlow = "low"
	direction_outhi  = "high"

	// the longest time to wait for an operation to complete
	timelimit = time.Second * 2

	low  = "0"
	high = "1"
)

type Event struct {
	Value     bool
	Timestamp time.Time
}

func (e *Event) String() string {
	return fmt.Sprintf("%v at %v", e.Value, e.Timestamp)
}

type GPIOPort interface {
	State() string
	IsEnabled() bool
	Enable() error
	Reset() error
	SetMode(GPIOMode) error
	IsOutput() (bool, error)
	SetValue(bool) error
	SetValues(ch <-chan bool) (<-chan error, error)
	Value() (bool, error)
	Values(buffersize int) (<-chan Event, error)
}

type gport struct {
	mu        sync.Mutex
	host      *pi
	port      int
	sport     string
	folder    string
	value     string
	direction string
	edge      string
	export    string
	unexport  string
	resetters []func()
}

func newGPIO(host *pi, port int) *gport {

	sport := fmt.Sprintf("%d", port)
	gpio := host.gpiodir
	folder := filepath.Join(gpio, fmt.Sprintf("gpio%s", sport))
	export := filepath.Join(gpio, "export")
	unexport := filepath.Join(gpio, "unexport")

	return &gport{
		mu:        sync.Mutex{},
		host:      host,
		port:      port,
		sport:     sport,
		folder:    folder,
		value:     filepath.Join(folder, "value"),
		direction: filepath.Join(folder, "direction"),
		edge:      filepath.Join(folder, "edge"),
		export:    export,
		unexport:  unexport,
		resetters: make([]func(), 0),
	}
}

func (p *gport) String() string {
	return p.folder
}

func (p *gport) IsEnabled() bool {

	defer p.unlock(p.lock())

	return checkFile(p.folder)
}

func (p *gport) Enable() error {

	defer p.unlock(p.lock())

	if checkFile(p.folder) {
		return nil
	}

	info("GPIO Enabling %v\n", p)

	if err := writeFile(p.export, p.sport); err != nil {
		return err
	}

	start := time.Now()

	// wait for folder to arrive....
	ch, err := awaitFileCreate(p.folder, timelimit)
	if err != nil {
		return err
	}
	if err := <-ch; err != nil {
		return err
	}

	// and for all control files to exist and be writable
	// there's an issue with timeouts perhaps.... but that's OK.
	// don't check value ... it can give "operation not permitted" error for an input GPIO when the
	// write is made - go can interpret that as a permissions error
	for _, fname := range []string{p.direction, p.edge} {
		for {
			remaining := timelimit - time.Since(start)
			info("GPIO Enabling %v checking file %v state (timeout limit %v)\n", p, fname, remaining)
			if checkFile(fname) {
				// exists, but check writable.... invalid data will be ignored(rejected), but permissions won't
				if err := writeFile(fname, " "); err == nil || !os.IsPermission(err) {
					info("GPIO Enabling %v file %v state OK\n", p, fname)
					break
				} else {
					info("GPIO Enabling %v file %v state %v\n", p, fname, err)
				}
			}
			remaining = timelimit - time.Since(start)
			select {
			case <-time.After(remaining):
				return fmt.Errorf("Timed out enabling GPIO %v - %v not yet writable", p.sport, fname)
			case <-time.After(pollInterval):
				// next cycle
			}
		}

	}

	info("GPIO Enabled %v\n", p)

	return nil
}

func (p *gport) Reset() error {

	defer p.unlock(p.lock())

	if !checkFile(p.folder) {
		// already reset
		return nil
	}
	info("GPIO Resetting  %v\n", p)
	for _, r := range p.resetters {
		// call the reset function
		r()
	}
	p.resetters = nil

	if err := writeFile(p.unexport, p.sport); err != nil {
		return err
	}
	ch, err := awaitFileRemove(p.folder, timelimit)
	if err != nil {
		return err
	}

	if err := <-ch; err != nil {
		return err
	}

	// wait for the file to be removed, and then return
	info("GPIO Reset  %v\n", p)
	return nil

}

// GPIOResetAsync will reset the specified port and only return when it is complete
// Configure will
func (p *gport) SetMode(mode GPIOMode) error {

	defer p.unlock(p.lock())

	err := p.checkEnabled()
	if err != nil {
		return err
	}

	direction := ""

	switch mode {
	case GPIOInput:
		direction = direction_in
	case GPIOOutput:
		direction = direction_out
	case GPIOOutputHigh:
		direction = direction_outhi
	case GPIOOutputLow:
		direction = direction_outlow
	default:
		return fmt.Errorf("GPIOMode %v does not exist")
	}

	info("GPIO Setting mode on  %v to %v\n", p, direction)

	if err := p.writeDirection(direction); err != nil {
		return err
	}
	info("GPIO Set mode on  %v to %v\n", p, direction)
	return nil
}

func (p *gport) IsOutput() (bool, error) {

	defer p.unlock(p.lock())

	err := p.checkEnabled()
	if err != nil {
		return false, err
	}
	d, err := p.readDirection()
	if err != nil {
		return false, err
	}
	return d != "in", nil
}

func (p *gport) State() string {

	defer p.unlock(p.lock())

	base := fmt.Sprintf("GPIO %v: ", p.sport)
	if !checkFile(p.folder) {
		return base + "Reset"
	}

	dir, err := p.readDirection()
	if err != nil {
		return fmt.Sprintf("%v%v", base, err)
	}
	val, err := p.readValue()
	if err != nil {
		return fmt.Sprintf("%v%v", base, err)
	}

	return fmt.Sprintf("%v %v with value %v", base, dir, val)
}

func (p *gport) Value() (bool, error) {

	defer p.unlock(p.lock())

	err := p.checkEnabled()
	if err != nil {
		return false, err
	}

	d, err := p.readValue()
	if err != nil {
		return false, err
	}

	return d == "1", nil
}

func (p *gport) SetValue(value bool) error {

	defer p.unlock(p.lock())

	err := p.checkEnabled()
	if err != nil {
		return err
	}

	info("GPIO Set Value on %v to %v\n", p, value)

	val := low
	if value {
		val = high
	}

	return p.writeValue(val)

}

func (p *gport) SetValues(ch <-chan bool) (<-chan error, error) {
	defer p.unlock(p.lock())

	info("GPIO Setting Values set channel on %v\n", p)

	err := p.checkEnabled()
	if err != nil {
		return nil, err
	}

	errch := make(chan error, 1)
	killer := make(chan bool, 1)
	cleaner := func() {
		close(killer)
	}
	p.resetters = append(p.resetters, cleaner)

	go func() {
		defer close(errch)
		for {
			select {
			case <-killer:
				return
			case v, ok := <-ch:
				if !ok {
					return
				}
				err := p.SetValue(v)
				if err != nil {
					errch <- err
					return
				}
			}
		}
	}()

	return errch, nil

}

func (p *gport) Values(buffersize int) (<-chan Event, error) {
	defer p.unlock(p.lock())

	info("GPIO Setting Value channel on %v\n", p)

	err := p.checkEnabled()
	if err != nil {
		return nil, err
	}

	err = p.writeEdge("both")
	if err != nil {
		return nil, err
	}

	ch, cleaner, err := buildMonitor(p.value, buffersize)
	if err != nil {
		return nil, err
	}
	p.resetters = append(p.resetters, cleaner)

	return ch, nil
}

func (p *gport) writeEdge(edges string) error {
	return writeFile(p.edge, edges)
}

func (p *gport) readEdge() (string, error) {
	return readFile(p.edge)
}

func (p *gport) writeDirection(direction string) error {
	return writeFile(p.direction, direction)
}

func (p *gport) readDirection() (string, error) {
	return readFile(p.direction)
}

func (p *gport) writeValue(value string) error {
	return writeFile(p.value, value)
}

func (p *gport) readValue() (string, error) {
	return readFile(p.value)
}

func (p *gport) checkEnabled() error {
	if checkFile(p.folder) {
		return nil
	}
	return fmt.Errorf("GPIO %v is not enabled", p.port)
}

func (p *gport) lock() bool {
	p.mu.Lock()
	return true
}

func (p *gport) unlock(bool) {
	p.mu.Unlock()
}
