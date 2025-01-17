package checker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/karimsa/patrol/internal/history"
	"github.com/karimsa/patrol/internal/logger"
)

var (
	cmdShell = os.Getenv("SHELL")
)

func init() {
	if cmdShell == "" {
		cmdShell = "/bin/sh"
	}
	if cmdShell == "/bin/sh" {
		if str, err := os.Readlink(cmdShell); err == nil && strings.Contains(str, "dash") {
			cmdShell = "/bin/bash"
		}
	}
	log.Printf("Initializing with SHELL = %s", cmdShell)
}

type Checker struct {
	Group         string
	Name          string
	Type          string
	Cmd           string
	MetricUnit    string
	Interval      time.Duration
	CmdTimeout    time.Duration
	MaxRetries    int
	RetryInterval time.Duration
	History       *history.File

	logger   logger.Logger
	doneChan chan bool
	wg       *sync.WaitGroup
}

func New(c *Checker) *Checker {
	if c.CmdTimeout.Milliseconds() == 0 {
		c.CmdTimeout = 1 * time.Minute
	}
	c.doneChan = make(chan bool, 1)
	c.wg = &sync.WaitGroup{}
	c.SetLogLevel(logger.LevelInfo)
	if c.History != nil {
		c.History.AddChecker(c)
	}
	if c.MaxRetries == 0 {
		c.MaxRetries = 1
	}
	if c.RetryInterval == 0 {
		c.RetryInterval = 5 * time.Second
	}
	return c
}

func (c *Checker) GetGroup() string {
	return c.Group
}

func (c *Checker) GetName() string {
	return c.Name
}

func (c *Checker) SetLogLevel(level logger.LogLevel) {
	c.logger = logger.New(
		level,
		fmt.Sprintf("%s:%s:", c.Group, c.Name),
	)
}

func (c *Checker) Check() history.Item {
	var item history.Item
	for i := 0; i < c.MaxRetries; i++ {
		if i > 0 {
			c.logger.Debugf("Checker failed, retrying in %s", c.RetryInterval)
			select {
			case <-time.After(c.RetryInterval):
			case <-c.doneChan:
				return item
			}
		}
		item = c.check()
		if item.Status != "unhealthy" {
			return item
		}
	}
	return item
}

func (c *Checker) check() history.Item {
	c.logger.Debugf("Checking status")

	stdout := bytes.Buffer{}
	stderr := bytes.Buffer{}
	combinedOutput := bytes.Buffer{}

	ctx, cancel := context.WithTimeout(
		context.TODO(),
		c.CmdTimeout,
	)
	cmd := exec.CommandContext(
		ctx,
		cmdShell,
		"-o",
		"pipefail",
		"-ec",
		c.Cmd,
	)
	cmd.Stdin = os.Stdin
	cmd.Stdout = io.MultiWriter(&stdout, &combinedOutput)
	cmd.Stderr = io.MultiWriter(&stderr, &combinedOutput)

	cmdStart := time.Now()
	err := cmd.Run()
	cancel()

	item := history.Item{
		Group:      c.Group,
		Name:       c.Name,
		Type:       c.Type,
		Output:     combinedOutput.Bytes(),
		CreatedAt:  time.Now(),
		Duration:   time.Since(cmdStart),
		Metric:     0,
		MetricUnit: c.MetricUnit,
		Status:     "",
		Error:      "",
	}

	if exitErr, ok := err.(*exec.ExitError); err != nil && ok {
		item.Status = "unhealthy"
		item.Error = fmt.Sprintf("Process exited with status %d", exitErr.ExitCode())
	} else if err != nil {
		item.Status = "unhealthy"
		item.Error = fmt.Sprintf("Failed to run: #%v", err)
	} else {
		item.Status = "healthy"

		if c.Type == "metric" {
			n, err := strconv.ParseFloat(strings.TrimSpace(string(stdout.Bytes())), 10)
			if err == nil {
				item.Metric = n
			} else {
				item.Status = "unhealthy"
				item.Error = fmt.Sprintf("Failed to parse metric from output: %s", err)
			}
		}
	}

	c.logger.Infof("Check completed: %s", item)
	return item
}

type eventReceiver interface {
	OnCheckerStatus(status, service, check string)
}

func (c *Checker) Start(receiver eventReceiver) error {
	c.wg.Add(1)
	go func() {
		defer func() {
			c.logger.Debugf("Checker stopped")
			c.wg.Done()
		}()

		for {
			item := c.Check()

			// Only perform write if the 'Close()' was not called already
			select {
			case <-c.doneChan:
				c.logger.Debugf("Skipping write, checker is closed")

			default:
				var err error
				item, err = c.History.Append(item)
				if err != nil {
					panic(err)
				}
				if receiver != nil {
					receiver.OnCheckerStatus(item.Status, item.Group, item.Name)
				}
			}

			c.logger.Infof("Waiting %s before checking again", c.Interval)
			select {
			case <-time.After(c.Interval):
			case <-c.doneChan:
				return
			}
		}
	}()
	return nil
}

func (c *Checker) Close() {
	close(c.doneChan)
	c.wg.Wait()
}
