package connectors

import "errors"

func ValidateCredentialSource(source, kind, mode string) error {
	switch source {
	case "", "operator":
		return nil
	case "caller":
		if kind != KindPlugin && (mode == "api_key" || mode == "headers" || mode == "static") {
			return nil
		}
		return errors.New("Caller credentials require API-key, declared-header, or static AWS authentication on a built-in connector.")
	default:
		return errors.New("Choose operator or caller credentials.")
	}
}
