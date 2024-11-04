package logging

import "github.com/sirupsen/logrus"

var log = logrus.WithField("prefix", "attestation")
