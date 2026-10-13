package transactionlog

import (
	"encoding/json"

	"go.uber.org/zap"
)

// Log writes the shared event as top-level structured fields.
func Log(logger *zap.Logger, event Event) {
	if logger == nil {
		return
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		logger.Error("transaction log encode failed", zap.String("service", event.Service))
		return
	}
	var values map[string]any
	if err := json.Unmarshal(encoded, &values); err != nil {
		logger.Error("transaction log decode failed", zap.String("service", event.Service))
		return
	}
	fields := make([]zap.Field, 0, len(values))
	for key, value := range values {
		if key != "timestamp" && key != "level" && key != "msg" {
			fields = append(fields, zap.Any(key, value))
		}
	}
	switch event.Level {
	case "error":
		logger.Error(event.Message, fields...)
	case "warn":
		logger.Warn(event.Message, fields...)
	default:
		logger.Info(event.Message, fields...)
	}
}
