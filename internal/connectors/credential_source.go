package connectors

import "errors"

func ValidateCredentialSource(source string, kind Kind, mode AuthMode) error {
	switch source {
	case "", "operator":
		return nil
	case "caller":
		if kind != KindPlugin && (mode == AuthAPIKey || mode == AuthHeaders || mode == AuthStatic) {
			return nil
		}
		return errors.New("Caller credentials require API-key, declared-header, or static AWS authentication on a built-in connector.")
	default:
		return errors.New("Choose operator or caller credentials.")
	}
}
