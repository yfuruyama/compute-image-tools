//  Copyright 2017 Google Inc. All Rights Reserved.
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package daisy

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"sync"
	"time"
)

// CreateInstances is a Daisy CreateInstances workflow step.
type CreateInstances []*Instance

func logSerialOutput(ctx context.Context, s *Step, i *Instance, port int64, interval time.Duration) {
	w := s.w
	w.logWait.Add(1)
	defer w.logWait.Done()

	logsObj := path.Join(w.logsPath, fmt.Sprintf("%s-serial-port%d.log", i.Name, port))
	w.LogStepInfo(s.name, "CreateInstances", "Streaming instance %q serial port %d output to https://storage.cloud.google.com/%s/%s", i.Name, port, w.bucket, logsObj)
	var start int64
	var buf bytes.Buffer
	var gcsErr bool
	tick := time.Tick(interval)

Loop:
	for {
		select {
		case <-tick:
			resp, err := w.ComputeClient.GetSerialPortOutput(path.Base(i.Project), path.Base(i.Zone), i.Name, port, start)
			if err != nil {
				// Instance is stopped or stopping.
				status, sErr := w.ComputeClient.InstanceStatus(path.Base(i.Project), path.Base(i.Zone), i.Name)
				switch status {
				case "TERMINATED", "STOPPED", "STOPPING":
					if sErr == nil {
						break Loop
					}
				}
				w.LogStepInfo(s.name, "CreateInstances", "Instance %q: error getting serial port: %v", i.Name, err)
				break Loop
			}
			start = resp.Next
			buf.WriteString(resp.Contents)
			wc := w.StorageClient.Bucket(w.bucket).Object(logsObj).NewWriter(ctx)
			wc.ContentType = "text/plain"
			if _, err := wc.Write(buf.Bytes()); err != nil && !gcsErr {
				gcsErr = true
				w.LogStepInfo(s.name, "CreateInstances", "Instance %q: error writing log to GCS: %v", i.Name, err)
				continue
			} else if err != nil { // dont try to close the writer
				continue
			}
			if err := wc.Close(); err != nil && !gcsErr {
				gcsErr = true
				w.LogStepInfo(s.name, "CreateInstances", "Instance %q: error saving log to GCS: %v", i.Name, err)
				continue
			}

			if w.isCanceled() {
				break Loop
			}
		}
	}

	w.Logger.WriteSerialPortLogs(w, i.Name, buf)
}

// populate preprocesses fields: Name, Project, Zone, Description, MachineType, NetworkInterfaces, Scopes, ServiceAccounts, and daisyName.
// - sets defaults
// - extends short partial URLs to include "projects/<project>"
func (c *CreateInstances) populate(ctx context.Context, s *Step) DError {
	var errs DError
	for _, i := range *c {
		errs = addErrs(errs, i.populate(ctx, s))
	}
	return errs
}

func (c *CreateInstances) validate(ctx context.Context, s *Step) DError {
	var errs DError
	for _, i := range *c {
		errs = addErrs(errs, i.validate(ctx, s))
	}
	return errs
}

func (c *CreateInstances) run(ctx context.Context, s *Step) DError {
	var wg sync.WaitGroup
	w := s.w
	eChan := make(chan DError)
	for _, ci := range *c {
		wg.Add(1)
		go func(i *Instance) {
			defer wg.Done()

			for _, d := range i.Disks {
				if diskRes, ok := w.disks.get(d.Source); ok {
					d.Source = diskRes.link
				}
				if d.InitializeParams != nil && d.InitializeParams.SourceImage != "" {
					if image, ok := w.images.get(d.InitializeParams.SourceImage); ok {
						d.InitializeParams.SourceImage = image.link
					}
				}
			}

			for _, n := range i.NetworkInterfaces {
				if netRes, ok := w.networks.get(n.Network); ok {
					n.Network = netRes.link
				}
				if subnetRes, ok := w.subnetworks.get(n.Subnetwork); ok {
					n.Subnetwork = subnetRes.link
				}
			}

			w.LogStepInfo(s.name, "CreateInstances", "Creating instance %q.", i.Name)
			if err := w.ComputeClient.CreateInstance(i.Project, i.Zone, &i.Instance); err != nil {
				eChan <- newErr("failed to create instances", err)
				return
			}
			go logSerialOutput(ctx, s, i, 1, 3*time.Second)
		}(ci)
	}

	go func() {
		wg.Wait()
		eChan <- nil
	}()

	select {
	case err := <-eChan:
		return err
	case <-w.Cancel:
		// Wait so instances being created now can be deleted.
		wg.Wait()
		return nil
	}
}
