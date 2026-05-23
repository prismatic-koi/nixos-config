// Package notify implements the org.freedesktop.Notifications client.
//
// The daemon talks to the session bus directly via godbus — no
// dunstify subprocess, no libnotify cgo. The Notifier interface lets
// tests inject a fake to verify the state machine wires actions to
// Notify/Close in the right order.
package notify

import (
	"errors"
	"fmt"

	"github.com/godbus/dbus/v5"
)

// Urgency mirrors the freedesktop notification urgency byte.
type Urgency byte

const (
	UrgencyLow      Urgency = 0
	UrgencyNormal   Urgency = 1
	UrgencyCritical Urgency = 2
)

// Notification carries the parameters of a single notify call.
type Notification struct {
	// AppName is shown by the notification daemon as the source app.
	AppName string
	// ReplacesID is the cookie returned by a previous Notify, or 0
	// to allocate a fresh notification. We use this for the
	// rolling low-battery bubble so repeated pings update in place.
	ReplacesID uint32
	// Icon is a freedesktop icon name (battery-low, battery-empty,
	// battery-full-charged, …). Resolved by the notification daemon.
	Icon    string
	Summary string
	Body    string
	Urgency Urgency
	// ExpireTimeout in ms. -1 means "the server decides" (dunst's
	// default), which mirrors the previous Python behaviour and
	// keeps critical notifications persistent until dismissed.
	ExpireTimeout int32
}

// Notifier is the interface the daemon depends on. Implementations:
// DBusNotifier (production) and tests' fake.
type Notifier interface {
	// Notify sends or replaces a notification. Returns the cookie
	// the server allocated for it (which the caller should pass in
	// ReplacesID on the next update).
	Notify(n Notification) (uint32, error)
	// Close closes a previously-sent notification by cookie. A zero
	// id is a no-op (so callers don't have to special-case "we
	// never sent anything").
	Close(id uint32) error
}

// DBusNotifier is the production Notifier. It owns a session-bus
// connection and addresses the org.freedesktop.Notifications object.
type DBusNotifier struct {
	conn *dbus.Conn
}

// NewDBus connects to the session bus and returns a Notifier.
func NewDBus() (*DBusNotifier, error) {
	conn, err := dbus.SessionBus()
	if err != nil {
		return nil, fmt.Errorf("connect session bus: %w", err)
	}
	return &DBusNotifier{conn: conn}, nil
}

// Close releases the session bus connection.
func (d *DBusNotifier) CloseConn() error {
	if d.conn == nil {
		return nil
	}
	return d.conn.Close()
}

const (
	notifBus    = "org.freedesktop.Notifications"
	notifPath   = "/org/freedesktop/Notifications"
	notifIface  = "org.freedesktop.Notifications"
	notifMethod = "org.freedesktop.Notifications.Notify"
	closeMethod = "org.freedesktop.Notifications.CloseNotification"
)

// Notify implements Notifier.
func (d *DBusNotifier) Notify(n Notification) (uint32, error) {
	if d.conn == nil {
		return 0, errors.New("notifier not connected")
	}
	obj := d.conn.Object(notifBus, notifPath)
	// org.freedesktop.Notifications.Notify signature:
	//   Notify(app_name s, replaces_id u, app_icon s, summary s,
	//          body s, actions as, hints a{sv}, expire_timeout i)
	// We send empty actions and hints — dunst respects urgency from
	// the dedicated arg below via the well-known "urgency" hint.
	hints := map[string]dbus.Variant{
		"urgency": dbus.MakeVariant(byte(n.Urgency)),
	}
	call := obj.Call(
		notifMethod, 0,
		n.AppName,
		n.ReplacesID,
		n.Icon,
		n.Summary,
		n.Body,
		[]string{}, // actions
		hints,
		n.ExpireTimeout,
	)
	if call.Err != nil {
		return 0, fmt.Errorf("Notify: %w", call.Err)
	}
	var id uint32
	if err := call.Store(&id); err != nil {
		return 0, fmt.Errorf("Notify reply: %w", err)
	}
	return id, nil
}

// Close implements Notifier.
func (d *DBusNotifier) Close(id uint32) error {
	if id == 0 {
		return nil
	}
	if d.conn == nil {
		return errors.New("notifier not connected")
	}
	obj := d.conn.Object(notifBus, notifPath)
	call := obj.Call(closeMethod, 0, id)
	if call.Err != nil {
		return fmt.Errorf("CloseNotification(%d): %w", id, call.Err)
	}
	return nil
}
