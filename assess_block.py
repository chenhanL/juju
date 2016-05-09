#!/usr/bin/env python
"""TODO: add rough description of what is assessed in this module."""

from __future__ import print_function

import argparse
import logging
import sys
import yaml

from deploy_stack import (
    BootstrapManager,
)
from utility import (
    add_basic_testing_arguments,
    configure_logging,
)


__metaclass__ = type


log = logging.getLogger("assess_block")


def get_block_list(client):
    return yaml.safe_load(client.get_juju_output(
        'block list', '--format', 'yaml'))


def assess_block(client):
    block_list = get_block_list(client)
    client.deploy('mediawiki-single')
    client.wait_for_started()
    if block_list != [
            {'block': 'destroy-model', 'enabled': False},
            {'block': 'remove-object', 'enabled': False},
            {'block': 'all-changes', 'enabled': False}]:
        raise AssertionError(block_list)
    client.juju('expose', ('mediawiki',))
    client.juju('block all-changes', ())
    block_list = get_block_list(client)
    if block_list != [
            {'block': 'destroy-model', 'enabled': False},
            {'block': 'remove-object', 'enabled': False},
            {'block': 'all-changes', 'enabled': True, 'message': ''}]:
        raise AssertionError(block_list)
    client.juju('unblock all-changes', ())
    block_list = get_block_list(client)
    if block_list != [
            {'block': 'destroy-model', 'enabled': False},
            {'block': 'remove-object', 'enabled': False},
            {'block': 'all-changes', 'enabled': False}]:
        raise AssertionError(block_list)


def parse_args(argv):
    """Parse all arguments."""
    parser = argparse.ArgumentParser(description="TODO: script info")
    # TODO: Add additional positional arguments.
    add_basic_testing_arguments(parser)
    # TODO: Add additional optional arguments.
    return parser.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)
    configure_logging(args.verbose)
    bs_manager = BootstrapManager.from_args(args)
    with bs_manager.booted_context(args.upload_tools):
        assess_block(bs_manager.client)
    return 0


if __name__ == '__main__':
    sys.exit(main())
