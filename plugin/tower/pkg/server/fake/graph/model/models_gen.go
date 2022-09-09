// Code generated by github.com/99designs/gqlgen, DO NOT EDIT.

package model

import (
	"fmt"
	"io"
	"strconv"

	"github.com/everoute/everoute/plugin/tower/pkg/schema"
)

type EverouteClusterEvent struct {
	Mutation       schema.MutationType     `json:"mutation"`
	Node           *schema.EverouteCluster `json:"node"`
	PreviousValues *schema.ObjectReference `json:"previousValues"`
}

type HostEvent struct {
	Mutation       schema.MutationType     `json:"mutation"`
	Node           *schema.Host            `json:"node"`
	PreviousValues *schema.ObjectReference `json:"previousValues"`
}

type IsolationPolicyEvent struct {
	Mutation       schema.MutationType     `json:"mutation"`
	Node           *schema.IsolationPolicy `json:"node"`
	PreviousValues *schema.ObjectReference `json:"previousValues"`
}

type LabelEvent struct {
	Mutation       schema.MutationType     `json:"mutation"`
	Node           *schema.Label           `json:"node"`
	PreviousValues *schema.ObjectReference `json:"previousValues"`
}

type Login struct {
	Token string `json:"token"`
}

type LoginInput struct {
	Password string     `json:"password"`
	Source   UserSource `json:"source"`
	Username string     `json:"username"`
}

type SecurityGroupEvent struct {
	Mutation       schema.MutationType     `json:"mutation"`
	Node           *schema.SecurityGroup   `json:"node"`
	PreviousValues *schema.ObjectReference `json:"previousValues"`
}

type SecurityPolicyEvent struct {
	Mutation       schema.MutationType     `json:"mutation"`
	Node           *schema.SecurityPolicy  `json:"node"`
	PreviousValues *schema.ObjectReference `json:"previousValues"`
}

type TaskEvent struct {
	Mutation       schema.MutationType     `json:"mutation"`
	Node           *schema.Task            `json:"node"`
	PreviousValues *schema.ObjectReference `json:"previousValues"`
}

type VMEvent struct {
	Mutation       schema.MutationType     `json:"mutation"`
	Node           *schema.VM              `json:"node"`
	PreviousValues *schema.ObjectReference `json:"previousValues"`
}

type TaskOrderByInput string

const (
	TaskOrderByInputLocalCreatedAtAsc  TaskOrderByInput = "local_created_at_ASC"
	TaskOrderByInputLocalCreatedAtDesc TaskOrderByInput = "local_created_at_DESC"
)

var AllTaskOrderByInput = []TaskOrderByInput{
	TaskOrderByInputLocalCreatedAtAsc,
	TaskOrderByInputLocalCreatedAtDesc,
}

func (e TaskOrderByInput) IsValid() bool {
	switch e {
	case TaskOrderByInputLocalCreatedAtAsc, TaskOrderByInputLocalCreatedAtDesc:
		return true
	}
	return false
}

func (e TaskOrderByInput) String() string {
	return string(e)
}

func (e *TaskOrderByInput) UnmarshalGQL(v interface{}) error {
	str, ok := v.(string)
	if !ok {
		return fmt.Errorf("enums must be strings")
	}

	*e = TaskOrderByInput(str)
	if !e.IsValid() {
		return fmt.Errorf("%s is not a valid TaskOrderByInput", str)
	}
	return nil
}

func (e TaskOrderByInput) MarshalGQL(w io.Writer) {
	fmt.Fprint(w, strconv.Quote(e.String()))
}

type UserSource string

const (
	UserSourceLdap  UserSource = "LDAP"
	UserSourceLocal UserSource = "LOCAL"
)

var AllUserSource = []UserSource{
	UserSourceLdap,
	UserSourceLocal,
}

func (e UserSource) IsValid() bool {
	switch e {
	case UserSourceLdap, UserSourceLocal:
		return true
	}
	return false
}

func (e UserSource) String() string {
	return string(e)
}

func (e *UserSource) UnmarshalGQL(v interface{}) error {
	str, ok := v.(string)
	if !ok {
		return fmt.Errorf("enums must be strings")
	}

	*e = UserSource(str)
	if !e.IsValid() {
		return fmt.Errorf("%s is not a valid UserSource", str)
	}
	return nil
}

func (e UserSource) MarshalGQL(w io.Writer) {
	fmt.Fprint(w, strconv.Quote(e.String()))
}
