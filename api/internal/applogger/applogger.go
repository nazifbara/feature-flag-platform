// Package applogger builds the application's logger, which redacts secrets
// and expands errors into their message, attributes and stack trace.
package applogger

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
	"github.com/nazifbara/feature-flag-platform/api/internal/apperr"
	pkgerr "github.com/pkg/errors"
	"gopkg.in/natefinch/lumberjack.v2"
)

// stackTracker is an error with a stack trace, such as one made by
// github.com/pkg/errors.
type stackTracker interface {
	error
	StackTrace() pkgerr.StackTrace
}

// multiError is an error wrapping several, such as one from errors.Join.
type multiError interface {
	error
	Unwrap() []error
}

// errorAttrs describes err for the log: its message, the causes its message
// leaves out, the attributes added with apperr.WithAttrs anywhere in its
// chain, and its stack trace if one of the errors it wraps has one. Like the
// causes and attributes, the stack trace is looked for above any join only,
// so a joined error's trace is logged on its own entry.
func errorAttrs(err error) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("message", err.Error()),
	}
	if causes := hiddenCauses(err); len(causes) > 0 {
		attrs = append(attrs, slog.String("cause", strings.Join(causes, ": ")))
	}
	attrs = append(attrs, apperr.Attrs(err)...)
	if trace, ok := stackTrace(err); ok {
		attrs = append(attrs, slog.Attr{
			Key:   "stack_trace",
			Value: slog.StringValue(fmt.Sprintf("%+v", trace)),
		})
	}
	return attrs
}

// stackTrace returns the stack trace of the first error in err's chain that
// has one. It follows single wrapping only, unlike errors.AsType, which also
// looks inside joins.
func stackTrace(err error) (pkgerr.StackTrace, bool) {
	for ; err != nil; err = errors.Unwrap(err) {
		if stackErr, ok := err.(stackTracker); ok {
			return stackErr.StackTrace(), true
		}
	}
	return nil, false
}

// hiddenCauses returns, outermost first, the text of each error in err's chain
// that the error wrapping it leaves out of its own text, as a connect.Error
// does with its cause. It follows single wrapping only.
func hiddenCauses(err error) []string {
	var causes []string
	for {
		inner := errors.Unwrap(err)
		if inner == nil {
			return causes
		}
		if !strings.Contains(err.Error(), inner.Error()) {
			causes = append(causes, inner.Error())
		}
		err = inner
	}
}

// redactURL replaces with "xxxxx" the password of every URL in s that has one,
// taking the URLs to be the space-separated words with a scheme and a host.
// The rest of s comes back unchanged.
func redactURL(s string) string {
	parts := strings.Split(s, " ")
	result := []string{}
	for _, part := range parts {
		purl, err := url.Parse(part)
		isURL := err == nil && purl.Scheme != "" && purl.Host != ""
		if !isURL {
			result = append(result, part)
			continue
		}
		if _, ok := purl.User.Password(); ok {
			purl.User = url.UserPassword(purl.User.Username(), "xxxxx")
			result = append(result, purl.String())
		} else {
			result = append(result, part)
		}
	}
	return strings.Join(result, " ")
}

// sensitiveKeys are the attribute keys whose values are never logged. They
// match whole keys, ignoring case, so "apiKey" matches "apikey" but
// "db_password" doesn't match "password".
var sensitiveKeys = []string{
	"password", "passwd", "password_hash", "new_password", "old_password",
	"apikey", "api_key", "secret", "client_secret",
	"token", "access_token", "refresh_token", "id_token", "session_token",
	"authorization", "cookie", "set-cookie",
	"pin", "creditcardno",
}

// replaceLogAttr is the handlers' ReplaceAttr. It redacts the values of
// sensitiveKeys and the passwords in URLs, and expands an error logged under
// "error" into a group from errorAttrs. When the error wraps several, it's an
// "errors" group instead, holding errorAttrs of the whole error and one entry
// per wrapped error. Only that first join is expanded: a join inside an entry
// is logged as its text.
func replaceLogAttr(groups []string, a slog.Attr) slog.Attr {
	if slices.Contains(sensitiveKeys, strings.ToLower(a.Key)) {
		return slog.String(a.Key, "[REDACTED]")
	}
	if a.Value.Kind() == slog.KindString {
		return slog.String(a.Key, redactURL(a.Value.String()))
	}
	if a.Key == "error" {
		err, ok := a.Value.Any().(error)
		if !ok {
			return a
		}
		meAttrs := errorAttrs(err)
		if me, ok := errors.AsType[multiError](err); ok {
			for i, err := range me.Unwrap() {
				meAttrs = append(meAttrs, slog.GroupAttrs(fmt.Sprintf("error_%d", i+1), errorAttrs(err)...))
			}
			return slog.GroupAttrs("errors", meAttrs...)
		}
		return slog.GroupAttrs("error", meAttrs...)
	}
	return a
}

// closeFunc releases what a logger writes to.
type closeFunc func() error

// InitializeLogger builds a logger that writes text from the debug level up to
// stderr, colored when stderr is a terminal. When logFile isn't empty, it also
// writes JSON from the info level up to logFile, rotated every megabyte with
// ten compressed backups kept. The returned closeFunc closes logFile.
func InitializeLogger(logFile string) (*slog.Logger, closeFunc, error) {
	var (
		handlers []slog.Handler
		closers  []closeFunc
	)
	handlers = append(handlers, tint.NewTextHandler(os.Stderr, &tint.Options{
		Level:       slog.LevelDebug,
		ReplaceAttr: replaceLogAttr,
		NoColor:     !(isatty.IsCygwinTerminal(os.Stderr.Fd()) || isatty.IsTerminal(os.Stderr.Fd())),
	}))

	if logFile != "" {
		fileHanlder := &lumberjack.Logger{
			Filename:   logFile,
			MaxSize:    1,
			MaxBackups: 10,
			LocalTime:  false,
			Compress:   true,
		}
		handlers = append(handlers, slog.NewJSONHandler(fileHanlder, &slog.HandlerOptions{
			Level:       slog.LevelInfo,
			ReplaceAttr: replaceLogAttr,
		}))
		closers = append(closers, fileHanlder.Close)
	}

	close := func() error {
		var errs []error
		for _, closer := range closers {
			errs = append(errs, closer())
		}
		return errors.Join(errs...)
	}

	return slog.New(slog.NewMultiHandler(handlers...)), close, nil
}
