package evergreen

import (
	"fmt"
	"text/template"

	"github.com/evergreen-ci/evergreen/db"
	"github.com/mongodb/grip"
	"github.com/pkg/errors"
)

type JIRANotificationsConfig struct {
	// CustomFields is a map[string]map[string]string. The key of the first
	// map is the JIRA project (ex: EVG), the key of the second map is
	// the custom field name, and the inner most value is the template
	// for the custom field
	CustomFields JIRACustomFieldsByProject `bson:"custom_fields"`
}

type JIRACustomFieldsByProject []JIRANotificationsProject

func (j JIRACustomFieldsByProject) ToMap() (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	projectDupes := map[string]bool{}

	for _, project := range j {
		_, isDupe := projectDupes[project.Project]
		if isDupe {
			return nil, errors.Errorf("duplicate project key '%s'", project.Project)
		}
		projectDupes[project.Project] = true

		fieldDupes := map[string]bool{}
		fields := map[string]string{}
		for i := range project.Fields {
			_, isDupeField := fieldDupes[project.Fields[i].Field]
			if isDupeField {
				return nil, errors.Errorf("duplicate field key '%s' in project '%s'", project.Fields[i].Field, project.Project)
			}
			fieldDupes[project.Fields[i].Field] = true
			fields[project.Fields[i].Field] = project.Fields[i].Template
		}

		out[project.Project] = fields
	}
	return out, nil
}

func (j *JIRACustomFieldsByProject) FromMap(m map[string]map[string]string) {
	*j = make(JIRACustomFieldsByProject, 0, len(m))
	for project, fields := range m {
		fieldsSlice := make([]JIRANotificationsCustomField, 0, len(fields))
		for field, tmpl := range fields {
			fieldsSlice = append(fieldsSlice, JIRANotificationsCustomField{
				Field:    field,
				Template: tmpl,
			})
		}
		*j = append(*j, JIRANotificationsProject{
			Project: project,
			Fields:  fieldsSlice,
		})
	}
}

type JIRANotificationsProject struct {
	Project string                         `bson:"project"`
	Fields  []JIRANotificationsCustomField `bson:"fields"`
}

type JIRANotificationsCustomField struct {
	Field    string `bson:"field"`
	Template string `bson:"template"`
}

func (c *JIRANotificationsConfig) SectionId() string { return "jira_notifications" }

func (c *JIRANotificationsConfig) Get() error {
	err := db.FindOneQ(ConfigCollection, db.Query(byId(c.SectionId())), c)
	if err != nil && err.Error() == errNotFound {
		*c = JIRANotificationsConfig{}
		return nil
	}

	return errors.Wrapf(err, "error retrieving section %s", c.SectionId())
}

func (c *JIRANotificationsConfig) Set() error {
	_, err := db.Upsert(ConfigCollection, byId(c.SectionId()), c)
	return errors.Wrapf(err, "error updating section %s", c.SectionId())
}

func (c *JIRANotificationsConfig) ValidateAndDefault() error {
	catcher := grip.NewSimpleCatcher()
	m, err := c.CustomFields.ToMap()
	if err != nil {
		return errors.Wrap(err, "failed to build jira notifications custom field")
	}

	for project, fields := range m {
		for field, tmpl := range fields {
			_, err := template.New(fmt.Sprintf("%s-%s", project, field)).Parse(tmpl)
			catcher.Add(err)
		}
	}
	return catcher.Resolve()
}
