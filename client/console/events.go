package console

/*
   Sliver Implant Framework
   Copyright (C) 2019  Bishop Fox

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU General Public License as published by
   the Free Software Foundation, either version 3 of the License, or
   (at your option) any later version.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU General Public License for more details.

   You should have received a copy of the GNU General Public License
   along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	consts "github.com/bishopfox/sliver/client/constants"
	"github.com/bishopfox/sliver/client/core"
	"github.com/bishopfox/sliver/client/prelude"
	"github.com/bishopfox/sliver/protobuf/clientpb"
	"github.com/bishopfox/sliver/protobuf/commonpb"
	"github.com/gofrs/uuid"
	"google.golang.org/protobuf/proto"
)

func (con *SliverClient) startEventLoop() {
	eventStream, err := con.Rpc.Events(context.Background(), &commonpb.Empty{})
	if err != nil {
		fmt.Printf(Warn+"%s\n", err)
		return
	}
	for {
		event, err := eventStream.Recv()
		if err == io.EOF || event == nil {
			return
		}

		go con.triggerEventListeners(event)

		// Trigger event based on type
		switch event.EventType {

		case consts.CanaryEvent:
			con.PrintEventErrorf(Bold+"WARNING: %s%s has been burned (DNS Canary)", Normal, event.Session.Name)
			sessions := con.GetSessionsByName(event.Session.Name)
			for _, session := range sessions {
				shortID := strings.Split(session.ID, "-")[0]
				con.PrintErrorf("\t🔥 Session %s is affected", shortID)
			}

		case consts.WatchtowerEvent:
			msg := string(event.Data)
			con.PrintEventErrorf(Bold+"WARNING: %s%s has been burned (seen on %s)", Normal, event.Session.Name, msg)
			sessions := con.GetSessionsByName(event.Session.Name)
			for _, session := range sessions {
				shortID := strings.Split(session.ID, "-")[0]
				con.PrintErrorf("\t🔥 Session %s is affected", shortID)
			}

		case consts.JoinedEvent:
			if con.Settings.UserConnect {
				con.PrintInfof("%s has joined the game", event.Client.Operator.Name)
			}
		case consts.LeftEvent:
			if con.Settings.UserConnect {
				con.PrintInfof("%s left the game", event.Client.Operator.Name)
			}

		case consts.JobStoppedEvent:
			job := event.Job
			con.PrintErrorf("Job #%d stopped (%s/%s)", job.ID, job.Protocol, job.Name)

		case consts.SessionOpenedEvent:
			session := event.Session
			currentTime := time.Now().Format(time.RFC1123)
			shortID := strings.Split(session.ID, "-")[0]
			con.PrintEventInfof("Session %s %s - %s (%s) - %s/%s - %v",
				shortID, session.Name, session.RemoteAddress, session.Hostname, session.OS, session.Arch, currentTime)

			// Prelude Operator
			if prelude.ImplantMapper != nil {
				err = prelude.ImplantMapper.AddImplant(session, nil)
				if err != nil {
					con.PrintErrorf("Could not add session to Operator: %s", err)
				}
			}

		case consts.SessionUpdateEvent:
			session := event.Session
			currentTime := time.Now().Format(time.RFC1123)
			shortID := strings.Split(session.ID, "-")[0]
			con.PrintInfof("Session %s has been updated - %v", shortID, currentTime)

		case consts.SessionClosedEvent:
			session := event.Session
			currentTime := time.Now().Format(time.RFC1123)
			shortID := strings.Split(session.ID, "-")[0]
			con.PrintEventErrorf("Lost session %s %s - %s (%s) - %s/%s - %v",
				shortID, session.Name, session.RemoteAddress, session.Hostname, session.OS, session.Arch, currentTime)
			activeSession := con.ActiveTarget.GetSession()
			core.GetTunnels().CloseForSession(session.ID)
			core.CloseCursedProcesses(session.ID)
			if activeSession != nil && activeSession.ID == session.ID {
				con.ActiveTarget.Set(nil, nil)
				con.PrintErrorf("Active session disconnected")
			}
			if prelude.ImplantMapper != nil {
				err = prelude.ImplantMapper.RemoveImplant(session)
				if err != nil {
					con.PrintErrorf("Could not remove session from Operator: %s", err)
				}
				con.PrintInfof("Removed session %s from Operator", session.Name)
			}

		case consts.BeaconRegisteredEvent:
			beacon := &clientpb.Beacon{}
			proto.Unmarshal(event.Data, beacon)
			currentTime := time.Now().Format(time.RFC1123)
			shortID := strings.Split(beacon.ID, "-")[0]
			con.PrintEventInfof("Beacon %s %s - %s (%s) - %s/%s - %v",
				shortID, beacon.Name, beacon.RemoteAddress, beacon.Hostname, beacon.OS, beacon.Arch, currentTime)

			// Prelude Operator
			if prelude.ImplantMapper != nil {
				err = prelude.ImplantMapper.AddImplant(beacon, func(taskID string, cb func(*clientpb.BeaconTask)) {
					con.AddBeaconCallback(taskID, cb)
				})
				if err != nil {
					con.PrintErrorf("Could not add beacon to Operator: %s", err)
				}
			}

		case consts.BeaconTaskResultEvent:
			con.triggerBeaconTaskCallback(event.Data)
		}

		con.triggerReactions(event)
	}
}

// CreateEventListener - creates a new event listener and returns its ID.
func (con *SliverClient) CreateEventListener() (string, <-chan *clientpb.Event) {
	listener := make(chan *clientpb.Event, 100)
	listenerID, _ := uuid.NewV4()
	con.EventListeners.Store(listenerID.String(), listener)
	return listenerID.String(), listener
}

// RemoveEventListener - removes an event listener given its id.
func (con *SliverClient) RemoveEventListener(listenerID string) {
	value, ok := con.EventListeners.LoadAndDelete(listenerID)
	if ok {
		close(value.(chan *clientpb.Event))
	}
}

func (con *SliverClient) triggerEventListeners(event *clientpb.Event) {
	con.EventListeners.Range(func(key, value interface{}) bool {
		listener := value.(chan *clientpb.Event)
		listener <- event // Do not block while sending the event to the listener
		return true
	})
}

func (con *SliverClient) triggerReactions(event *clientpb.Event) {
	reactions := core.Reactions.On(event.EventType)
	if len(reactions) == 0 {
		return
	}

	// We need some special handling for SessionOpenedEvent to
	// set the new session as the active session
	currentActiveSession, currentActiveBeacon := con.ActiveTarget.Get()
	defer func() {
		con.ActiveTarget.Set(currentActiveSession, currentActiveBeacon)
	}()

	if event.EventType == consts.SessionOpenedEvent {
		con.ActiveTarget.Set(nil, nil)

		con.ActiveTarget.Set(event.Session, nil)
	} else if event.EventType == consts.BeaconRegisteredEvent {
		con.ActiveTarget.Set(nil, nil)

		beacon := &clientpb.Beacon{}
		proto.Unmarshal(event.Data, beacon)
		con.ActiveTarget.Set(nil, beacon)
	}

	for _, reaction := range reactions {
		for _, line := range reaction.Commands {
			con.PrintInfof(Bold+"Execute reaction: '%s'"+Normal, line)
			err := con.App.ActiveMenu().RunCommand(line)
			if err != nil {
				con.PrintErrorf("Reaction command error: %s\n", err)
			}
		}
	}
}

// triggerBeaconTaskCallback - Triggers the callback for a beacon task.
func (con *SliverClient) triggerBeaconTaskCallback(data []byte) {
	task := &clientpb.BeaconTask{}
	err := proto.Unmarshal(data, task)
	if err != nil {
		con.PrintErrorf("\rCould not unmarshal beacon task: %s", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	beacon, _ := con.Rpc.GetBeacon(ctx, &clientpb.Beacon{ID: task.BeaconID})

	// If the callback is not in our map then we don't do anything, the beacon task
	// was either issued by another operator in multiplayer mode or the client process
	// was restarted between the time the task was created and when the server got the result
	con.BeaconTaskCallbacksMutex.Lock()
	defer con.BeaconTaskCallbacksMutex.Unlock()
	if callback, ok := con.BeaconTaskCallbacks[task.ID]; ok {
		if con.Settings.BeaconAutoResults {
			if beacon != nil {
				con.PrintSuccessf("%s completed task %s", beacon.Name, strings.Split(task.ID, "-")[0])
			}
			task_content, err := con.Rpc.GetBeaconTaskContent(ctx, &clientpb.BeaconTask{
				ID: task.ID,
			})
			con.Printf(Clearln + "\r")
			if err == nil {
				callback(task_content)
			} else {
				con.PrintErrorf("Could not get beacon task content: %s", err)
			}
		}
		delete(con.BeaconTaskCallbacks, task.ID)
	}
}

func (con *SliverClient) AddBeaconCallback(taskID string, callback BeaconTaskCallback) {
	con.BeaconTaskCallbacksMutex.Lock()
	defer con.BeaconTaskCallbacksMutex.Unlock()
	con.BeaconTaskCallbacks[taskID] = callback
}