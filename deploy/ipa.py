#!/usr/bin/python3
"""Chaperone's FreeIPA objects: the broker's role and the eligibility group.

Run as root on a FreeIPA server (LDAPI autobind, no admin password needed):

  python3 ipa.py install   BROKER_HOST ELIGIBLE_GROUP
  python3 ipa.py retire    BROKER_HOST ELIGIBLE_GROUP --personas NAME ...
  python3 ipa.py uninstall BROKER_HOST ELIGIBLE_GROUP --personas NAME ...

install is idempotent. Eligibility membership is managed by FreeIPA administrators,
not Chaperone. A pre-existing eligibility group is used without modification;
a group created by Chaperone is empty until an administrator adds members. The broker service chaperone/BROKER_HOST may create
users and groups, and change members only of groups marked "chaperone:*"
(each persona's own group) and descriptions only of personas. BROKER_HOST's host principal may fetch the
broker keytab. install never re-enables disabled personas; reactivation requires explicit administrator action.
retire deletes the broker rights and disables only the listed personas
(userclass "chaperone:*"), keeping them and their groups (description "chaperone:*"): what
personas created stays owned by a named identity the human shares a group
with. uninstall deletes the personas and their groups too; run it after
`chaperone _uninstall` on the broker host.
"""
import sys

from ipalib import api, errors

PERMISSION = 'Chaperone persona group membership'
# user-add --noprivate rewrites the new user's description afterwards.
DESC_PERMISSION = 'Chaperone persona description'
PRIVILEGE = 'Chaperone persona provisioning'
ROLE = 'Chaperone broker'
MARK = 'chaperone:'
ELIGIBLE_DESC = 'May launch Chaperone personas'


def ours(entry, attr):
    return any(v.startswith(MARK) for v in entry.get(attr, []))


def main(action, host, eligible, *rest):
    selected = []
    if action == 'install':
        if rest:
            sys.exit(__doc__)
    elif action in ('retire', 'uninstall'):
        if rest[:1] != ('--personas',):
            sys.exit('retire/uninstall requires an explicit --personas list')
        selected = sorted(set(rest[1:]))
    else:
        sys.exit(__doc__)
    api.bootstrap(context='cli', in_server=True, confdir='/etc/ipa', debug=False)
    api.finalize()
    api.Backend.ldap2.connect(autobind=True)
    cmd = api.Command
    service = 'chaperone/' + host
    # Broker privileges must be scoped to this host: uninstalling one broker
    # must not remove the role/permissions of another broker in the same realm.
    permission = PERMISSION + ' for ' + host
    desc_permission = DESC_PERMISSION + ' for ' + host
    privilege = PRIVILEGE + ' for ' + host
    role = ROLE + ' for ' + host

    if action == 'install':
        for name, arg, kw in [
            ('group_add', eligible, dict(description=ELIGIBLE_DESC)),
            ('service_add', service, dict(force=True)),  # no DNS check; the host must exist in IPA
            ('permission_add', permission, dict(
                ipapermright=['write'], attrs=['member'], type='group',
                extratargetfilter=['(description=%s*)' % MARK])),
            ('permission_add', desc_permission, dict(
                ipapermright=['write'], attrs=['description'], type='user',
                extratargetfilter=['(userclass=%s*)' % MARK])),
            ('privilege_add', privilege, dict(description='Create Chaperone personas and their own groups')),
            ('role_add', role, dict(description='chaperone _provision on ' + host)),
        ]:
            try:
                cmd[name](arg, **kw)
                print('created', arg)
            except errors.DuplicateEntry:
                print('exists ', arg)
        # *_add_member style calls report "already a member" instead of raising.
        cmd.privilege_add_permission(privilege, permission=[
            'System: Add Users',
            'System: Add User to default group',
            'System: Read UPG Definition',  # user-add reads it even with --noprivate
            'System: Add Groups',
            permission, desc_permission])
        cmd.role_add_privilege(role, privilege=[privilege])
        cmd.role_add_member(role, service=[service])
        cmd.service_allow_create_keytab(service, host=[host])
        group = cmd.group_show(eligible, all=True)['result']
        if group.get('description', [''])[0] != ELIGIBLE_DESC:
            print('using externally managed eligibility group', eligible)
        return

    if action not in ('retire', 'uninstall'):
        sys.exit(__doc__)
    for name in selected:
        try:
            user = cmd.user_show(name, all=True)['result']
        except errors.NotFound:
            continue
        if not ours(user, 'userclass'):
            sys.exit('refusing foreign persona %s' % name)
        if action == 'retire':
            try:
                cmd.user_disable(user['uid'][0])
                print('disabled persona', user['uid'][0])
            except errors.AlreadyInactive:
                print('disabled already', user['uid'][0])
        else:
            cmd.user_del(user['uid'][0])
            print('deleted persona', user['uid'][0])
    if action == 'uninstall':
        for name in selected:
            try:
                group = cmd.group_show(name, all=True)['result']
            except errors.NotFound:
                continue
            if not ours(group, 'description'):
                sys.exit('refusing foreign group %s' % name)
            cmd.group_del(name)
            print('deleted group  ', name)
    for name, arg in [('role_del', role), ('privilege_del', privilege), ('permission_del', permission),
                      ('permission_del', desc_permission),
                      ('service_del', service)]:
        try:
            cmd[name](arg)
            print('deleted', arg)
        except errors.NotFound:
            print('absent ', arg)


if __name__ == '__main__':
    if len(sys.argv) < 4:
        sys.exit(__doc__)
    main(*sys.argv[1:])
