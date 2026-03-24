package main

import (
	"io"
	"log"
	"os"
)

type Loggers struct {
	Error  *log.Logger
	Status *log.Logger
}

func setupLoggers() (*Loggers, error) {
	errorFile, err := os.OpenFile("error.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		return nil, err
	}
	if _, err := errorFile.Stat(); err != nil {
		// no-op, file already opened
	}
	multiError := io.MultiWriter(errorFile, os.Stdout)
	errorLogger := log.New(multiError, "ERROR: ", log.Ldate|log.Ltime|log.Lshortfile)

	statusFile, err := os.OpenFile("status.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		return nil, err
	}
	multiStatus := io.MultiWriter(statusFile, os.Stdout)
	statusLogger := log.New(multiStatus, "STATUS: ", log.Ldate|log.Ltime)

	return &Loggers{
		Error:  errorLogger,
		Status: statusLogger,
	}, nil
}
