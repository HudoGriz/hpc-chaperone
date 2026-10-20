#!/usr/bin/python3
"""Chaperone's FreeIPA objects: the broker's role and the eligibility group.

Run as root on a FreeIPA server (LDAPI autobind, no admin password needed):

  python3 ipa.py install   BROKER_HOST ELIGIBLE_GROUP [USER ...]
  python3 ipa.py uninstall BROKER_HOST ELIGIBLE_GROUP

install is idempotent. The broker service chaperone/BROKER_HOST may create
users and groups, and change members only of groups marked "chaperone:*"
(each persona's own group). BROKER_HOST's host principal may fetch the
broker keytab. uninstall deletes all of that plus every persona and persona
group; run it after `chaperone _uninstall` on the broker host.
"""
import sys

from ipalib import api, errors

PERMISSION = 'Chaperone persona group membership'
PRIVILEGE = 'Chaperone persona provisioning'
ROLE = 'Chaperone broker'
MARK = 'chaperone:'


def main(action, host, eligible, *users):
    api.bootstrap(context='cli', in_server=True, confdir='/etc/ipa', debug=False)
    api.finalize()
    api.Backend.ldap2.connect(autobind=True)
    cmd = api.Command
    service = 'chaperone/' + host

    if action == 'install':
        for name, arg, kw in [
            ('group_add', eligible, dict(description='May launch Chaperone personas')),
            ('service_add', service, {}),
            ('permission_add', PERMISSION, dict(
                ipapermright=['write'], attrs=['member'], type='group',
                extratargetfilter=['(description=%s*)' % MARK])),
            ('privilege_add', PRIVILEGE, dict(description='Create Chaperone personas and their own groups')),
            ('role_add', ROLE, dict(description='chaperone-provisiond on ' + host)),
        ]:
            try:
                cmd[name](arg, **kw)
                print('created', arg)
            except errors.DuplicateEntry:
                print('exists ', arg)
        # *_add_member style calls report "already a member" instead of raising.
        cmd.privilege_add_permission(PRIVILEGE, permission=[
            'System: Add Users',
            'System: Add User to default group',
            'System: Read UPG Definition',  # user-add reads it even with --noprivate
            'System: Add Groups',
            PERMISSION])
        cmd.role_add_privilege(ROLE, privilege=[PRIVILEGE])
        cmd.role_add_member(ROLE, service=[service])
        cmd.service_allow_create_keytab(service, host=[host])
        if users:
            cmd.group_add_member(eligible, user=list(users))
        return

    if action != 'uninstall':
        sys.exit(__doc__)
    ours = lambda entry: any(d.startswith(MARK) for d in entry.get('description', []))
    for user in cmd.user_find(sizelimit=0, all=True)['result']:
        if ours(user):
            cmd.user_del(user['uid'][0])
            print('deleted persona', user['uid'][0])
    for group in cmd.group_find(sizelimit=0, all=True)['result']:
        if ours(group):
            cmd.group_del(group['cn'][0])
            print('deleted group  ', group['cn'][0])
    for name, arg in [('role_del', ROLE), ('privilege_del', PRIVILEGE), ('permission_del', PERMISSION),
                      ('service_del', service), ('group_del', eligible)]:
        try:
            cmd[name](arg)
            print('deleted', arg)
        except errors.NotFound:
            print('absent ', arg)


if __name__ == '__main__':
    if len(sys.argv) < 4:
        sys.exit(__doc__)
    main(*sys.argv[1:])
