// +build !windows

package termbox

import "os"
import "os/signal"
import "syscall"

// public API

// Initializes termbox library. This function should be called before any other functions.
// After successful initialization, the library must be finalized using 'Close' function.
//
// Example usage:
//      err := termbox.Init()
//      if err != nil {
//              panic(err)
//      }
//      defer termbox.Close()
func Init() error {
	// TODO: try os.Stdin and os.Stdout directly
	var err error

	// os.Create is confusing here, but it's just a shortcut for 'open'
	out, err = os.Create("/dev/tty")
	if err != nil {
		return err
	}
	in, err = os.Open("/dev/tty")
	if err != nil {
		return err
	}

	err = setup_term()
	if err != nil {
		return err
	}

	// we set two signal handlers, because input/output are not really
	// connected, but they both need to be aware of window size changes
	signal.Notify(sigwinch, syscall.SIGWINCH)

	err = tcgetattr(out.Fd(), &orig_tios)
	if err != nil {
		return err
	}

	tios := orig_tios
	tios.Iflag &^= syscall_IGNBRK | syscall_BRKINT | syscall_PARMRK |
		syscall_ISTRIP | syscall_INLCR | syscall_IGNCR |
		syscall_ICRNL | syscall_IXON
	tios.Oflag &^= syscall_OPOST
	tios.Lflag &^= syscall_ECHO | syscall_ECHONL | syscall_ICANON |
		syscall_ISIG | syscall_IEXTEN
	tios.Cflag &^= syscall_CSIZE | syscall_PARENB
	tios.Cflag |= syscall_CS8
	tios.Cc[syscall_VMIN] = 1
	tios.Cc[syscall_VTIME] = 0

	err = tcsetattr(out.Fd(), &tios)
	if err != nil {
		return err
	}

	out.WriteString(funcs[t_enter_ca])
	out.WriteString(funcs[t_enter_keypad])
	out.WriteString(funcs[t_hide_cursor])
	out.WriteString(funcs[t_clear_screen])

	termw, termh = get_term_size(out.Fd())
	back_buffer.init(termw, termh)
	front_buffer.init(termw, termh)
	back_buffer.clear()
	front_buffer.clear()

	go func() {
		buf := make([]byte, 128)
		for {
			n, _ := in.Read(buf)
			input_comm <- buf[:n]
			buf = (<-input_comm)[:128]
		}
	}()

	return nil
}

// Finalizes termbox library, should be called after successful initialization
// when termbox's functionality isn't required anymore.
func Close() {
	out.WriteString(funcs[t_show_cursor])
	out.WriteString(funcs[t_sgr0])
	out.WriteString(funcs[t_clear_screen])
	out.WriteString(funcs[t_exit_ca])
	out.WriteString(funcs[t_exit_keypad])
	tcsetattr(out.Fd(), &orig_tios)

	out.Close()
	// closing in will block on Darwin if we have an outstanding Read
	// (which we will always have).
	/*in.Close()*/
}

// Synchronizes the internal back buffer with the terminal.
func Flush() {
	// invalidate cursor position
	lastx = coord_invalid
	lasty = coord_invalid

	update_size_maybe()

	for y := 0; y < front_buffer.height; y++ {
		line_offset := y * front_buffer.width
		for x := 0; x < front_buffer.width; x++ {
			cell_offset := line_offset + x
			back := &back_buffer.cells[cell_offset]
			front := &front_buffer.cells[cell_offset]
			if *back == *front {
				continue
			}
			send_attr(back.Fg, back.Bg)
			send_char(x, y, back.Ch)
			*front = *back
		}
	}
	if !is_cursor_hidden(cursor_x, cursor_y) {
		write_cursor(cursor_x, cursor_y)
	}
	flush()
}

// Sets the position of the cursor. See also HideCursor().
func SetCursor(x, y int) {
	if is_cursor_hidden(cursor_x, cursor_y) && !is_cursor_hidden(x, y) {
		outbuf.WriteString(funcs[t_show_cursor])
	}

	if !is_cursor_hidden(cursor_x, cursor_y) && is_cursor_hidden(x, y) {
		outbuf.WriteString(funcs[t_hide_cursor])
	}

	cursor_x, cursor_y = x, y
	if !is_cursor_hidden(cursor_x, cursor_y) {
		write_cursor(cursor_x, cursor_y)
	}
}

// The shortcut for SetCursor(-1, -1).
func HideCursor() {
	SetCursor(cursor_hidden, cursor_hidden)
}

// Changes cell's parameters in the internal back buffer at the specified
// position.
func SetCell(x, y int, ch rune, fg, bg Attribute) {
	if x < 0 || x >= back_buffer.width {
		return
	}
	if y < 0 || y >= back_buffer.height {
		return
	}

	back_buffer.cells[y*back_buffer.width+x] = Cell{ch, fg, bg}
}

// Draws a string into the internal back buffer at the specified position.
// The string does not wrap.
func DrawString(x, y int, fg, bg Attribute, str string) {
	if y < 0 || y >= back_buffer.height {
		return
	}
	for offset, ch := range str {
		if x+offset < 0 {
			continue
		}
		if x+offset >= back_buffer.width {
			break
		}
		back_buffer.cells[y*back_buffer.width+x+offset] = Cell{ch, fg, bg}
	}
}

// Returns a slice into the termbox's back buffer. You can get its dimensions
// using 'Size' function. The slice remains valid as long as no 'Clear' or
// 'Flush' function calls were made after call to this function.
func CellBuffer() []Cell {
	return back_buffer.cells
}

// Wait for an event and return it. This is a blocking function call.
func PollEvent() Event {
	var event Event

	// try to extract event from input buffer, return on success
	event.Type = EventKey
	if extract_event(&event) {
		return event
	}

	for {
		select {
		case data := <-input_comm:
			inbuf = append(inbuf, data...)
			input_comm <- data
			if extract_event(&event) {
				return event
			}
		case <-sigwinch:
			event.Type = EventResize
			event.Width, event.Height = get_term_size(out.Fd())
			return event
		}
	}
	panic("unreachable")
}

// Returns the size of the internal back buffer (which is the same as
// terminal's window size in characters).
func Size() (int, int) {
	return termw, termh
}

// Clears the internal back buffer.
func Clear(fg, bg Attribute) {
	foreground, background = fg, bg
	update_size_maybe()
	back_buffer.clear()
}

// Sets termbox input mode. Termbox has two input modes:
//
// 1. Esc input mode. When ESC sequence is in the buffer and it doesn't match
// any known sequence. ESC means KeyEsc.
//
// 2. Alt input mode. When ESC sequence is in the buffer and it doesn't match
// any known sequence. ESC enables ModAlt modifier for the next keyboard event.
//
// If 'mode' is InputCurrent, returns the current input mode. See also Input*
// constants.
func SetInputMode(mode InputMode) InputMode {
	if mode != InputCurrent {
		input_mode = mode
	}
	return input_mode
}
