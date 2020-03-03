package tview

import (
	"sync"
	"time"

	"github.com/gdamore/tcell"
)

// The size of the event/update/redraw channels.
const queueSize = 100

// Application represents the top node of an application.
//
// It is not strictly required to use this class as none of the other classes
// depend on it. However, it provides useful tools to set up an application and
// plays nicely with all widgets.
//
// The following command displays a primitive p on the screen until Ctrl-C is
// pressed:
//
//   if err := tview.NewApplication().SetRoot(p, true).Run(); err != nil {
//       panic(err)
//   }
type Application struct {
	sync.RWMutex

	// The application's screen. Apart from Run(), this variable should never be
	// set directly. Always use the screenReplacement channel after calling
	// Fini(), to set a new screen (or nil to stop the application).
	screen tcell.Screen

	// The primitive which currently has the keyboard focus.
	focus Primitive

	// The root primitive to be seen on the screen.
	root Primitive

	// Whether or not the application resizes the root primitive.
	rootFullscreen bool

	// Enable mouse events?
	enableMouse bool

	// An optional capture function which receives a key event and returns the
	// event to be forwarded to the default input handler (nil if nothing should
	// be forwarded).
	inputCapture func(event *tcell.EventKey) *tcell.EventKey

	// An optional callback function which is invoked just before the root
	// primitive is drawn.
	beforeDraw func(screen tcell.Screen) bool

	// An optional callback function which is invoked after the root primitive
	// was drawn.
	afterDraw func(screen tcell.Screen)

	// Used to send screen events from separate goroutine to main event loop
	events chan tcell.Event

	// Functions queued from goroutines, used to serialize updates to primitives.
	updates chan func()

	// An object that the screen variable will be set to after Fini() was called.
	// Use this channel to set a new screen object for the application
	// (screen.Init() and draw() will be called implicitly). A value of nil will
	// stop the application.
	screenReplacement chan tcell.Screen

	// An optional capture function which receives a mouse event and returns the
	// event to be forwarded to the default mouse handler (nil if nothing should
	// be forwarded).
	mouseCapture func(event *tcell.EventMouse, action MouseAction) (*tcell.EventMouse, MouseAction)

	// An optional mouse capture Primitive returned from the MouseHandler.
	mouseHandlerCapture Primitive

	lastMouseX, lastMouseY int // track last mouse pos
	mouseDownX, mouseDownY int // track last mouse down pos
	lastClickTime          time.Time
	lastMouseBtn           tcell.ButtonMask
}

// NewApplication creates and returns a new application.
func NewApplication() *Application {
	return &Application{
		events:            make(chan tcell.Event, queueSize),
		updates:           make(chan func(), queueSize),
		screenReplacement: make(chan tcell.Screen, 1),
	}
}

// SetInputCapture sets a function which captures all key events before they are
// forwarded to the key event handler of the primitive which currently has
// focus. This function can then choose to forward that key event (or a
// different one) by returning it or stop the key event processing by returning
// nil.
//
// Note that this also affects the default event handling of the application
// itself: Such a handler can intercept the Ctrl-C event which closes the
// application.
func (a *Application) SetInputCapture(capture func(event *tcell.EventKey) *tcell.EventKey) *Application {
	a.inputCapture = capture
	return a
}

// GetInputCapture returns the function installed with SetInputCapture() or nil
// if no such function has been installed.
func (a *Application) GetInputCapture() func(event *tcell.EventKey) *tcell.EventKey {
	return a.inputCapture
}

// SetMouseCapture sets a function which captures mouse events before they are
// forwarded to the appropriate mouse event handler.
//  This function can then choose to forward that event (or a
// different one) by returning it or stop the event processing by returning
// nil.
func (a *Application) SetMouseCapture(capture func(event *tcell.EventMouse, action MouseAction) (*tcell.EventMouse, MouseAction)) *Application {
	a.mouseCapture = capture
	return a
}

// GetMouseCapture returns the function installed with SetMouseCapture() or nil
// if no such function has been installed.
func (a *Application) GetMouseCapture() func(event *tcell.EventMouse, action MouseAction) (*tcell.EventMouse, MouseAction) {
	return a.mouseCapture
}

// SetScreen allows you to provide your own tcell.Screen object. For most
// applications, this is not needed and you should be familiar with
// tcell.Screen when using this function.
//
// This function is typically called before the first call to Run(). Init() need
// not be called on the screen.
func (a *Application) SetScreen(screen tcell.Screen) *Application {
	if screen == nil {
		return a // Invalid input. Do nothing.
	}

	a.Lock()
	if a.screen == nil {
		// Run() has not been called yet.
		a.screen = screen
		a.Unlock()
		return a
	}

	// Run() is already in progress. Exchange screen.
	oldScreen := a.screen
	a.Unlock()
	oldScreen.Fini()
	a.screenReplacement <- screen

	return a
}

// EnableMouse enables mouse events.
func (a *Application) EnableMouse(enable bool) *Application {
	a.Lock()
	defer a.Unlock()
	if enable != a.enableMouse && a.screen != nil {
		if enable {
			a.screen.EnableMouse()
		} else {
			a.screen.DisableMouse()
		}
	}
	a.enableMouse = enable
	return a
}

// Run starts the application and thus the event loop. This function returns
// when Stop() was called.
func (a *Application) Run() error {
	var err error
	a.Lock()

	// Make a screen if there is none yet.
	if a.screen == nil {
		a.screen, err = tcell.NewScreen()
		if err != nil {
			a.Unlock()
			return err
		}
		if err = a.screen.Init(); err != nil {
			a.Unlock()
			return err
		}
		if a.enableMouse {
			a.screen.EnableMouse()
		}
	}

	// We catch panics to clean up because they mess up the terminal.
	defer func() {
		if p := recover(); p != nil {
			if a.screen != nil {
				a.screen.Fini()
			}
			panic(p)
		}
	}()

	// Draw the screen for the first time.
	a.Unlock()
	a.draw()

	// Separate loop to wait for screen events.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			a.RLock()
			screen := a.screen
			a.RUnlock()
			if screen == nil {
				// We have no screen. Let's stop.
				a.QueueEvent(nil)
				break
			}

			// Wait for next event and queue it.
			event := screen.PollEvent()
			if event != nil {
				// Regular event. Queue.
				a.QueueEvent(event)
				continue
			}

			// A screen was finalized (event is nil). Wait for a new scren.
			screen = <-a.screenReplacement
			if screen == nil {
				// No new screen. We're done.
				a.QueueEvent(nil)
				return
			}

			// We have a new screen. Keep going.
			a.Lock()
			a.screen = screen
			a.Unlock()

			// Initialize and draw this screen.
			if err := screen.Init(); err != nil {
				panic(err)
			}
			a.draw()
		}
	}()

	// Start event loop.
EventLoop:
	for {
		select {
		case event := <-a.events:
			if event == nil {
				break EventLoop
			}

			a.RLock()
			p := a.focus
			inputCapture := a.inputCapture
			mouseCapture := a.mouseCapture
			screen := a.screen
			root := a.root
			a.RUnlock()

			switch event := event.(type) {
			case *tcell.EventKey:
				// Intercept keys.
				if inputCapture != nil {
					event = inputCapture(event)
					if event == nil {
						a.draw()
						continue // Don't forward event.
					}
				}

				// Ctrl-C closes the application.
				if event.Key() == tcell.KeyCtrlC {
					a.Stop()
				}

				// Pass other key events to the currently focused primitive.
				if p != nil {
					if handler := p.InputHandler(); handler != nil {
						handler(event, func(p Primitive) {
							a.SetFocus(p)
						})
						a.draw()
					}
				}
			case *tcell.EventResize:
				if screen == nil {
					continue
				}
				screen.Clear()
				a.draw()
			case *tcell.EventMouse:
				isMouseDownAction := false
				// Fire a mouse action.
				mouseEv := func(action MouseAction) {
					switch action {
					case MouseLeftDown, MouseMiddleDown, MouseRightDown:
						isMouseDownAction = true
					}

					// Intercept event.
					if mouseCapture != nil {
						event, action = mouseCapture(event, action)
						if event == nil {
							a.draw()
							return // Don't forward event.
						}
					}

					var newHandlerCapture Primitive   // None by default.
					if a.mouseHandlerCapture != nil { // Check if already captured.
						if handler := a.mouseHandlerCapture.MouseHandler(); handler != nil {
							_, newHandlerCapture = handler(action, event, func(p Primitive) {
								a.SetFocus(p)
							})
							a.draw()
						}
					} else if handler := root.MouseHandler(); handler != nil {
						_, newHandlerCapture = handler(action, event, func(p Primitive) {
							a.SetFocus(p)
						})
						a.draw()
					}
					a.mouseHandlerCapture = newHandlerCapture
				}

				a.fireMouseActions(event, mouseEv)

				// Keep state:
				a.lastMouseBtn = event.Buttons()
				if isMouseDownAction {
					a.mouseDownX, a.mouseDownY = event.Position()
				}
			}

		// If we have updates, now is the time to execute them.
		case updater := <-a.updates:
			updater()
		}
	}

	// Wait for the event loop to finish.
	wg.Wait()
	a.screen = nil

	return nil
}

// fireMouseActions determines each mouse action from mouse events
// and fires the appropriate mouse handlers and mouse captures.
func (a *Application) fireMouseActions(event *tcell.EventMouse, mouseEv func(MouseAction)) {
	atX, atY := event.Position()
	btn := event.Buttons()
	clickMoved := atX != a.mouseDownX || atY != a.mouseDownY
	btnDiff := btn ^ a.lastMouseBtn

	if atX != a.lastMouseX || atY != a.lastMouseY {
		mouseEv(MouseMove)
		a.lastMouseX = atX
		a.lastMouseY = atY
	}

	if btnDiff&tcell.Button1 != 0 {
		if btn&tcell.Button1 != 0 {
			mouseEv(MouseLeftDown)
		} else {
			mouseEv(MouseLeftUp)
			if !clickMoved {
				if a.lastClickTime.Add(DoubleClickInterval).Before(time.Now()) {
					mouseEv(MouseLeftClick)
					a.lastClickTime = time.Now()
				} else {
					mouseEv(MouseLeftDoubleClick)
					a.lastClickTime = time.Time{} // reset
				}
			}
		}
	}

	if btnDiff&tcell.Button2 != 0 {
		if btn&tcell.Button2 != 0 {
			mouseEv(MouseMiddleDown)
		} else {
			mouseEv(MouseMiddleUp)
			if !clickMoved {
				if a.lastClickTime.Add(DoubleClickInterval).Before(time.Now()) {
					mouseEv(MouseMiddleClick)
					a.lastClickTime = time.Now()
				} else {
					mouseEv(MouseMiddleDoubleClick)
					a.lastClickTime = time.Time{} // reset
				}
			}
		}
	}

	if btnDiff&tcell.Button3 != 0 {
		if btn&tcell.Button3 != 0 {
			mouseEv(MouseRightDown)
		} else {
			mouseEv(MouseRightUp)
			if !clickMoved {
				if a.lastClickTime.Add(DoubleClickInterval).Before(time.Now()) {
					mouseEv(MouseRightClick)
					a.lastClickTime = time.Now()
				} else {
					mouseEv(MouseRightDoubleClick)
					a.lastClickTime = time.Time{} // reset
				}
			}
		}
	}

	if btn&tcell.WheelUp != 0 {
		mouseEv(WheelUp)
	}
	if btn&tcell.WheelDown != 0 {
		mouseEv(WheelDown)
	}
	if btn&tcell.WheelLeft != 0 {
		mouseEv(WheelLeft)
	}
	if btn&tcell.WheelRight != 0 {
		mouseEv(WheelRight)
	}
}

// Stop stops the application, causing Run() to return.
func (a *Application) Stop() {
	a.Lock()
	defer a.Unlock()
	screen := a.screen
	if screen == nil {
		return
	}
	a.screen = nil
	screen.Fini()
	a.screenReplacement <- nil
}

// Suspend temporarily suspends the application by exiting terminal UI mode and
// invoking the provided function "f". When "f" returns, terminal UI mode is
// entered again and the application resumes.
//
// A return value of true indicates that the application was suspended and "f"
// was called. If false is returned, the application was already suspended,
// terminal UI mode was not exited, and "f" was not called.
func (a *Application) Suspend(f func()) bool {
	a.RLock()
	screen := a.screen
	a.RUnlock()
	if screen == nil {
		return false // Screen has not yet been initialized.
	}

	// Enter suspended mode.
	screen.Fini()

	// Wait for "f" to return.
	f()

	// Make a new screen.
	var err error
	screen, err = tcell.NewScreen()
	if err != nil {
		panic(err)
	}
	a.screenReplacement <- screen
	// One key event will get lost, see https://github.com/gdamore/tcell/issues/194

	// Continue application loop.
	return true
}

// Draw refreshes the screen (during the next update cycle). It calls the Draw()
// function of the application's root primitive and then syncs the screen
// buffer.
func (a *Application) Draw() *Application {
	a.QueueUpdate(func() {
		a.draw()
	})
	return a
}

// ForceDraw refreshes the screen immediately. Use this function with caution as
// it may lead to race conditions with updates to primitives in other
// goroutines. It is always preferrable to use Draw() instead. Never call this
// function from a goroutine.
//
// It is safe to call this function during queued updates and direct event
// handling.
func (a *Application) ForceDraw() *Application {
	return a.draw()
}

// draw actually does what Draw() promises to do.
func (a *Application) draw() *Application {
	a.Lock()
	defer a.Unlock()

	screen := a.screen
	root := a.root
	fullscreen := a.rootFullscreen
	before := a.beforeDraw
	after := a.afterDraw

	// Maybe we're not ready yet or not anymore.
	if screen == nil || root == nil {
		return a
	}

	// Resize if requested.
	if fullscreen && root != nil {
		width, height := screen.Size()
		root.SetRect(0, 0, width, height)
	}

	// Call before handler if there is one.
	if before != nil {
		if before(screen) {
			screen.Show()
			return a
		}
	}

	// Draw all primitives.
	root.Draw(screen)

	// Call after handler if there is one.
	if after != nil {
		after(screen)
	}

	// Sync screen.
	screen.Show()

	return a
}

// SetBeforeDrawFunc installs a callback function which is invoked just before
// the root primitive is drawn during screen updates. If the function returns
// true, drawing will not continue, i.e. the root primitive will not be drawn
// (and an after-draw-handler will not be called).
//
// Note that the screen is not cleared by the application. To clear the screen,
// you may call screen.Clear().
//
// Provide nil to uninstall the callback function.
func (a *Application) SetBeforeDrawFunc(handler func(screen tcell.Screen) bool) *Application {
	a.beforeDraw = handler
	return a
}

// GetBeforeDrawFunc returns the callback function installed with
// SetBeforeDrawFunc() or nil if none has been installed.
func (a *Application) GetBeforeDrawFunc() func(screen tcell.Screen) bool {
	return a.beforeDraw
}

// SetAfterDrawFunc installs a callback function which is invoked after the root
// primitive was drawn during screen updates.
//
// Provide nil to uninstall the callback function.
func (a *Application) SetAfterDrawFunc(handler func(screen tcell.Screen)) *Application {
	a.afterDraw = handler
	return a
}

// GetAfterDrawFunc returns the callback function installed with
// SetAfterDrawFunc() or nil if none has been installed.
func (a *Application) GetAfterDrawFunc() func(screen tcell.Screen) {
	return a.afterDraw
}

// SetRoot sets the root primitive for this application. If "fullscreen" is set
// to true, the root primitive's position will be changed to fill the screen.
//
// This function must be called at least once or nothing will be displayed when
// the application starts.
//
// It also calls SetFocus() on the primitive.
func (a *Application) SetRoot(root Primitive, fullscreen bool) *Application {
	a.Lock()
	a.root = root
	a.rootFullscreen = fullscreen
	if a.screen != nil {
		a.screen.Clear()
	}
	a.Unlock()

	a.SetFocus(root)

	return a
}

// ResizeToFullScreen resizes the given primitive such that it fills the entire
// screen.
func (a *Application) ResizeToFullScreen(p Primitive) *Application {
	a.RLock()
	width, height := a.screen.Size()
	a.RUnlock()
	p.SetRect(0, 0, width, height)
	return a
}

// SetFocus sets the focus on a new primitive. All key events will be redirected
// to that primitive. Callers must ensure that the primitive will handle key
// events.
//
// Blur() will be called on the previously focused primitive. Focus() will be
// called on the new primitive.
func (a *Application) SetFocus(p Primitive) *Application {
	a.Lock()
	if a.focus != nil {
		a.focus.Blur()
	}
	a.focus = p
	if a.screen != nil {
		a.screen.HideCursor()
	}
	a.Unlock()
	if p != nil {
		p.Focus(func(p Primitive) {
			a.SetFocus(p)
		})
	}

	return a
}

// GetFocus returns the primitive which has the current focus. If none has it,
// nil is returned.
func (a *Application) GetFocus() Primitive {
	a.RLock()
	defer a.RUnlock()
	return a.focus
}

// QueueUpdate is used to synchronize access to primitives from non-main
// goroutines. The provided function will be executed as part of the event loop
// and thus will not cause race conditions with other such update functions or
// the Draw() function.
//
// Note that Draw() is not implicitly called after the execution of f as that
// may not be desirable. You can call Draw() from f if the screen should be
// refreshed after each update. Alternatively, use QueueUpdateDraw() to follow
// up with an immediate refresh of the screen.
func (a *Application) QueueUpdate(f func()) *Application {
	a.updates <- f
	return a
}

// QueueUpdateDraw works like QueueUpdate() except it refreshes the screen
// immediately after executing f.
func (a *Application) QueueUpdateDraw(f func()) *Application {
	a.QueueUpdate(func() {
		f()
		a.draw()
	})
	return a
}

// QueueEvent sends an event to the Application event loop.
//
// It is not recommended for event to be nil.
func (a *Application) QueueEvent(event tcell.Event) *Application {
	a.events <- event
	return a
}

// MouseAction indicates one of the actions the mouse is logically doing.
type MouseAction int16

const (
	MouseMove MouseAction = iota
	MouseLeftDown
	MouseLeftUp
	MouseLeftClick
	MouseLeftDoubleClick
	MouseMiddleDown
	MouseMiddleUp
	MouseMiddleClick
	MouseMiddleDoubleClick
	MouseRightDown
	MouseRightUp
	MouseRightClick
	MouseRightDoubleClick
	WheelUp
	WheelDown
	WheelLeft
	WheelRight
)

var DoubleClickInterval = 500 * time.Millisecond
