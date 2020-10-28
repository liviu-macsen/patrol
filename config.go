package patrol

import (
	"fmt"
	"io/ioutil"
	"time"

	"github.com/karimsa/patrol/internal/checker"
	"github.com/karimsa/patrol/internal/history"
	"github.com/karimsa/patrol/internal/logger"
	"gopkg.in/yaml.v2"
)

type notificationsRaw struct {
	OnFailure []struct {
		Type    string
		Options interface{}
	} `yaml:"on_failure"`
	OnSuccess []struct {
		Type    string
		Options interface{}
	} `yaml:"on_success"`
}

type configRaw struct {
	Name     string
	Port     int
	DB       string `yaml:"db"`
	LogLevel string `yaml:"logLevel"`
	Services map[string]struct {
		Checks []struct {
			Name       string
			Interval   duration
			Timeout    duration
			Cmd        string
			Type       string
			MetricUnit string `yaml:"unit"`
		}
		Notifications notificationsRaw
	}
	Notifications notificationsRaw
}

func FromConfigFile(filePath string, historyOptions *history.NewOptions) (*Patrol, configRaw, error) {
	buffer, err := ioutil.ReadFile(filePath)
	if err != nil {
		return nil, configRaw{}, err
	}
	return FromConfig(buffer, historyOptions)
}

func FromConfig(data []byte, historyOptions *history.NewOptions) (patrol *Patrol, raw configRaw, err error) {
	err = yaml.UnmarshalStrict(data, &raw)
	if err != nil {
		return
	}

	if raw.Name == "" {
		raw.Name = "Statuspage"
	}
	if raw.Port <= 0 {
		raw.Port = 80
	}
	if raw.DB == "" {
		err = fmt.Errorf("'db' propery must be specified in config file")
		return
	}
	if raw.LogLevel == "" {
		raw.LogLevel = "info"
	}
	logLevel, err := getLogLevel(raw.LogLevel)
	if err != nil {
		return
	}

	patrolOpts := CreatePatrolOptions{
		Name:     raw.Name,
		Port:     uint32(raw.Port),
		LogLevel: logLevel,
	}

	if historyOptions == nil {
		patrolOpts.History.File = raw.DB
	} else {
		patrolOpts.History = *historyOptions
	}

	// Just a random guess for size, estimating about 5 checks for
	// each defined service
	patrolOpts.Checkers = make([]*checker.Checker, 0, len(raw.Services)*5)

	historyFile, err := history.New(patrolOpts.History)
	if err != nil {
		return
	}

	if len(raw.Services) == 0 {
		err = fmt.Errorf("Config file contains no services")
		return
	}
	for group, groupConfig := range raw.Services {
		if groupConfig.Checks == nil || len(groupConfig.Checks) == 0 {
			err = fmt.Errorf("Empty group '%s' defined in config", group)
			return
		}

		for idx, checkConfig := range groupConfig.Checks {
			if checkConfig.Type == "" {
				checkConfig.Type = "boolean"
			}
			if checkConfig.Name == "" {
				err = fmt.Errorf("%d-th check missing name in %s", idx, group)
				return
			}
			if checkConfig.Cmd == "" {
				err = fmt.Errorf("%d-th check missing cmd in %s", idx, group)
				return
			}
			if checkConfig.Type == "metric" && checkConfig.MetricUnit == "" {
				err = fmt.Errorf("%d-th check is of type metric but is missing unit in %s", idx, group)
				return
			}
			if checkConfig.Interval.isZero() {
				checkConfig.Interval = duration(60 * time.Second)
			}
			if checkConfig.Timeout.isZero() {
				checkConfig.Timeout = duration(3 * time.Minute)
			}

			groupConfig.Checks[idx] = checkConfig
			patrolOpts.Checkers = append(patrolOpts.Checkers, checker.New(&checker.Checker{
				Group:      group,
				Name:       checkConfig.Name,
				Type:       checkConfig.Type,
				Cmd:        checkConfig.Cmd,
				MetricUnit: checkConfig.MetricUnit,
				Interval:   checkConfig.Interval.duration(),
				CmdTimeout: checkConfig.Timeout.duration(),
				History:    historyFile,
			}))
		}
	}

	patrol, err = New(patrolOpts, historyFile)
	return
}

func getLogLevel(level string) (logger.LogLevel, error) {
	switch level {
	case "none":
		return logger.LevelNone, nil
	case "info":
		return logger.LevelInfo, nil
	case "debug":
		return logger.LevelDebug, nil
	default:
		return logger.LogLevel(-1), fmt.Errorf("Unrecognized log level: '%s'", level)
	}
}
