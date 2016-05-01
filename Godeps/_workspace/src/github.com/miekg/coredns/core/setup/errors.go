package setup

import (
	"io"
	"log"
	"os"

	"github.com/miekg/coredns/middleware"
	"github.com/miekg/coredns/middleware/errors"

	"github.com/hashicorp/go-syslog"
)

// Errors configures a new errors middleware instance.
func Errors(c *Controller) (middleware.Middleware, error) {
	handler, err := errorsParse(c)
	if err != nil {
		return nil, err
	}

	var writer io.Writer

	switch handler.LogFile {
	case "visible":
		handler.Debug = true
	case "stdout":
		writer = os.Stdout
	case "stderr":
		writer = os.Stderr
	case "syslog":
		writer, err = gsyslog.NewLogger(gsyslog.LOG_ERR, "LOCAL0", "coredns")
		if err != nil {
			return nil, err
		}
	default:
		if handler.LogFile == "" {
			writer = os.Stderr // default
			break
		}

		var file *os.File
		file, err = os.OpenFile(handler.LogFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
		if err != nil {
			return nil, err
		}
		if handler.LogRoller != nil {
			file.Close()

			handler.LogRoller.Filename = handler.LogFile

			writer = handler.LogRoller.GetLogWriter()
		} else {
			writer = file
		}
	}
	handler.Log = log.New(writer, "", 0)

	return func(next middleware.Handler) middleware.Handler {
		handler.Next = next
		return handler
	}, nil
}

func errorsParse(c *Controller) (errors.ErrorHandler, error) {
	handler := errors.ErrorHandler{}

	optionalBlock := func() (bool, error) {
		var hadBlock bool

		for c.NextBlock() {
			hadBlock = true

			what := c.Val()
			if !c.NextArg() {
				return hadBlock, c.ArgErr()
			}
			where := c.Val()

			if what == "log" {
				if where == "visible" {
					handler.Debug = true
				} else {
					handler.LogFile = where
					if c.NextArg() {
						if c.Val() == "{" {
							c.IncrNest()
							logRoller, err := parseRoller(c)
							if err != nil {
								return hadBlock, err
							}
							handler.LogRoller = logRoller
						}
					}
				}
			}
		}
		return hadBlock, nil
	}

	for c.Next() {
		// weird hack to avoid having the handler values overwritten.
		if c.Val() == "}" {
			continue
		}
		// Configuration may be in a block
		hadBlock, err := optionalBlock()
		if err != nil {
			return handler, err
		}

		// Otherwise, the only argument would be an error log file name or 'visible'
		if !hadBlock {
			if c.NextArg() {
				if c.Val() == "visible" {
					handler.Debug = true
				} else {
					handler.LogFile = c.Val()
				}
			}
		}
	}

	return handler, nil
}
