#!/usr/bin/python3
"""Chaperone's FreeIPA objects: the broker's role and the eligibility group.

Run as root on a FreeIPA server (LDAPI autobind, no admin password needed):

  python3 ipa.py install   BROKER_HOST ELIGIBLE_GROUP [--members USER ...] [--ban GROUP ...]
  python3 ipa.py retire    BROKER_HOST ELIGIBLE_GROUP
  python3 ipa.py uninstall BROKER_HOST ELIGIBLE_GROUP

install is idempotent. With --members the eligibility group holds exactly
those users afterwards; without it, its members are left alone. A group of
that name not made by Chaperone is refused, and uninstall leaves it. The
broker service chaperone/BROKER_HOST may create users and groups, change
members of every group except the --ban ones (personas get their human's
groups minus those), and descriptions only of personas. BROKER_HOST's host
principal may fetch the broker keytab. install re-enables retired personas.
retire deletes all of that and disables every persona (userclass
"chaperone:*"), keeping it and its group (description "chaperone:*"): what
personas created stays owned by a named identity the human shares a group
with. uninstall deletes the personas and their groups too; run it after
their jobs and sessions are stopped.
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


def personas(cmd):
    return [u for u in cmd.user_find(sizelimit=0, all=True)['result'] if ours(u, 'userclass')]


def main(action, host, eligible, *rest):
    opts, key = {}, None
    for a in rest:
        if a in ('--members', '--ban'):
            key = opts.setdefault(a, [])
        elif key is None:
            sys.exit(__doc__)
        else:
            key.append(a)
    members, ban = opts.get('--members'), opts.get('--ban', [])
    # FreeIPA enforces the ban against the broker too.
    member_filter = ['(!(|%s))' % ''.join('(cn=%s)' % g for g in ban)] if ban else None
    api.bootstrap(context='cli', in_server=True, confdir='/etc/ipa', debug=False)
    api.finalize()
    api.Backend.ldap2.connect(autobind=True)
    cmd = api.Command
    service = 'chaperone/' + host

    if action == 'install':
        # Refused before any change: a refused run must not reset the ban.
        try:
            if cmd.group_show(eligible)['result'].get('description', [''])[0] != ELIGIBLE_DESC:
                sys.exit('group %s exists and is not Chaperone\'s; choose another chaperone_eligible_group' % eligible)
        except errors.NotFound:
            pass
        for name, arg, kw in [
            ('group_add', eligible, dict(description=ELIGIBLE_DESC)),
            ('service_add', service, dict(force=True)),  # no DNS check; the host must exist in IPA
            ('permission_add', PERMISSION, dict(
                ipapermright=['write'], attrs=['member'], type='group', extratargetfilter=member_filter)),
            ('permission_add', DESC_PERMISSION, dict(
                ipapermright=['write'], attrs=['description'], type='user',
                extratargetfilter=['(userclass=%s*)' % MARK])),
            ('privilege_add', PRIVILEGE, dict(description='Create Chaperone personas and their own groups')),
            ('role_add', ROLE, dict(description='chaperone _provision on ' + host)),
        ]:
            try:
                cmd[name](arg, **kw)
                print('created', arg)
            except errors.DuplicateEntry:
                print('exists ', arg)
        try:  # the ban list may have changed since
            cmd.permission_mod(PERMISSION, extratargetfilter=member_filter)
            print('updated', PERMISSION)
        except errors.EmptyModlist:
            pass
        # *_add_member style calls report "already a member" instead of raising.
        cmd.privilege_add_permission(PRIVILEGE, permission=[
            'System: Add Users',
            'System: Add User to default group',
            'System: Read UPG Definition',  # user-add reads it even with --noprivate
            'System: Add Groups',
            PERMISSION, DESC_PERMISSION])
        cmd.role_add_privilege(ROLE, privilege=[PRIVILEGE])
        cmd.role_add_member(ROLE, service=[service])
        cmd.service_allow_create_keytab(service, host=[host])
        for user in personas(cmd):  # retired by an earlier retire
            if user.get('nsaccountlock', False) in (True, 'TRUE', ['TRUE'], ('TRUE',)):
                cmd.user_enable(user['uid'][0])
                print('enabled persona', user['uid'][0])
        if members is not None:
            current = set(cmd.group_show(eligible)['result'].get('member_user', []))
            add, drop = sorted(set(members) - current), sorted(current - set(members))
            for verb, users in (('add', add), ('remove', drop)):
                if users:
                    r = cmd['group_%s_member' % verb](eligible, user=users)
                    failed = r['failed']['member']['user']
                    if failed:
                        sys.exit('group_%s_member %s: %s' % (verb, eligible, failed))
                    print('member %s %s' % (verb, ' '.join(users)))
        return

    if action not in ('retire', 'uninstall'):
        sys.exit(__doc__)
    for user in personas(cmd):
        if action == 'retire':
            try:
                cmd.user_disable(user['uid'][0])
                print('disabled persona', user['uid'][0])
            except errors.AlreadyInactive:
                print('disabled already', user['uid'][0])
        else:
            cmd.user_del(user['uid'][0])
            print('deleted persona', user['uid'][0])
    for group in cmd.group_find(sizelimit=0, all=True)['result'] if action == 'uninstall' else []:
        if ours(group, 'description'):
            cmd.group_del(group['cn'][0])
            print('deleted group  ', group['cn'][0])
    try:
        ours_eligible = cmd.group_show(eligible)['result'].get('description', [''])[0] == ELIGIBLE_DESC
    except errors.NotFound:
        ours_eligible = False
    for name, arg in [('role_del', ROLE), ('privilege_del', PRIVILEGE), ('permission_del', PERMISSION),
                      ('permission_del', DESC_PERMISSION),
                      ('service_del', service)] + ([('group_del', eligible)] if ours_eligible else []):
        try:
            cmd[name](arg)
            print('deleted', arg)
        except errors.NotFound:
            print('absent ', arg)


if __name__ == '__main__':
    if len(sys.argv) < 4:
        sys.exit(__doc__)
    main(*sys.argv[1:])
