// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022 Datadog, Inc.
package utils

import (
	"encoding/json"
	"net/mail"

	"github.com/DataDog/chaos-controller/api/v1beta1"
	"github.com/DataDog/chaos-controller/eventnotifier/types"
	v1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
)

// BuildBodyMessageFromDisruptionEvent Templated body text to send to notifiers
func BuildBodyMessageFromDisruptionEvent(dis v1beta1.Disruption, event corev1.Event, isMarkdown bool) string {
	if isMarkdown {
		return "> Disruption `" + dis.Name + "` emitted the event `" + event.Reason + "`: " + event.Message
	}

	return "Disruption '" + dis.Name + "' emitted the event " + event.Reason + ": " + event.Message
}

// BuildHeaderMessageFromDisruptionEvent Templated header text to send to notifiers
func BuildHeaderMessageFromDisruptionEvent(dis v1beta1.Disruption, notifType types.NotificationType) string {
	switch notifType {
	case types.NotificationInfo:
		return "Disruption '" + dis.Name + "' received a notification."
	case types.NotificationSuccess:
		return "Disruption '" + dis.Name + "' received a recovery notification."
	default:
		return "Disruption '" + dis.Name + "' encountered an issue."
	}
}

func GetUserInfoFromDisruption(dis v1beta1.Disruption) (*mail.Address, error) {
	var annotation v1.UserInfo

	err := json.Unmarshal([]byte(dis.Annotations["UserInfo"]), &annotation)
	if err != nil {
		return nil, err
	}

	return mail.ParseAddress(annotation.Username)
}
