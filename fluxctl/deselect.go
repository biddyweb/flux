package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/squaremo/flux/common/store"
)

type deselectOpts struct {
	store store.Store
}

func (opts *deselectOpts) addCommandTo(top *cobra.Command) {
	top.AddCommand(&cobra.Command{
		Use:   "deselect <service> <rule>",
		Short: "remove a selection rule from a service",
		Long:  "Remove selection rule <rule> from <service>. Containers may still be selected by other rules.",
		Run:   opts.run,
	})
}

func (opts *deselectOpts) run(_ *cobra.Command, args []string) {
	if len(args) != 2 {
		exitWithErrorf("Expected <service> and <rule>")
	}
	serviceName, group := args[0], args[1]

	// Check that the service exists
	_, err := opts.store.GetServiceDetails(serviceName)
	if err != nil {
		exitWithErrorf("Error fetching service: ", err)
	}

	if err = opts.store.RemoveContainerGroupSpec(serviceName, group); err != nil {
		exitWithErrorf("Unable to update service %s: %s", serviceName, err)
	}

	fmt.Printf("Removed rule %s from service %s\n", group, serviceName)
}