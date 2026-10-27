package config

import "github.com/khanhicetea/minicrond/internal/model"

func ValidateDefinition(d *model.Definition, jobDefaults ...model.Definition) error {
	return validateDefinition(d, explicit{}, jobDefaults)
}
