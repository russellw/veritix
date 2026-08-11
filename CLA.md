# Veritix Contributor Licence Agreement

**Version 1.0.**

## Why this exists

Veritix is dual licensed: AGPL-3.0-or-later to everyone, and on commercial
terms to anyone who needs terms the AGPL cannot give (see
[`LICENSING.md`](LICENSING.md)). The project can only offer a commercial
licence for code it holds the rights to offer it for. Code contributed under
the AGPL alone could not go into a commercially licensed build, which would
mean either turning contributions away or quietly shipping them anyway. This
agreement is the honest version of that.

**This is a licence, not an assignment.** You keep the copyright in everything
you write. You keep the right to use your own contribution however you like,
including in other projects and under other licences. What you grant is
permission for the Project to use it — including in a commercially licensed
build — and nothing you grant here is exclusive.

You should be comfortable with the whole of it before you sign. If you are
not, say so; a contribution that cannot be licensed this way can still be
useful as a bug report, a failing test, or a description of the fix.

## Agreement

By signing this agreement, You accept and agree to the following terms for
Your present and future Contributions to the Project. Except for the licences
granted here, You reserve all right, title and interest in and to Your
Contributions.

### 1. Definitions

**"You"** means the individual who signs this agreement, or the legal entity
on whose behalf it is signed. For a legal entity, the entity and all entities
that control, are controlled by, or are under common control with it are
treated as a single Contributor.

**"Project"** means Veritix, the software published at
`https://github.com/russellwallace/veritix`.

**"Maintainer"** means Russell Wallace, the copyright holder of the Project,
and any successor to whom the Project's copyright is transferred.

**"Contribution"** means any work of authorship — source code, documentation,
tests, configuration, fixtures, or anything else — that You intentionally
submit to the Project for inclusion in it. "Submit" means any form of
electronic, verbal, or written communication sent to the Maintainer or to the
Project's repositories, issue tracker, or discussion channels, including a
pull request, a patch, or a commit pushed to a branch of the Project. It
excludes anything You conspicuously mark, in writing, as "Not a Contribution".

### 2. Copyright licence

You grant to the Maintainer a perpetual, worldwide, non-exclusive, no-charge,
royalty-free and irrevocable copyright licence to reproduce, prepare
derivative works of, publicly display, publicly perform, sublicense and
distribute Your Contributions and such derivative works.

That licence expressly includes the right to sublicense and distribute Your
Contributions, and works derived from them, **under any licence terms,
including the AGPL, other open-source licences, and proprietary or commercial
terms** — with or without a requirement to make source code available.

### 3. Patent licence

You grant to the Maintainer and to recipients of software distributed by the
Maintainer a perpetual, worldwide, non-exclusive, no-charge, royalty-free and
irrevocable (except as stated in this section) patent licence to make, have
made, use, offer to sell, sell, import and otherwise transfer Your
Contributions. This licence extends only to those patent claims licensable by
You that are necessarily infringed by Your Contributions alone or by the
combination of Your Contributions with the Project.

If any entity institutes patent litigation against You or any other entity
alleging that Your Contribution, or the Project to which You contributed,
constitutes direct or contributory patent infringement, then any patent
licence granted to that entity under this agreement for that Contribution or
Project terminates as of the date such litigation is filed.

### 4. Your representations

You represent that:

a. Each of Your Contributions is Your original creation, or You otherwise have
   the legal right to grant the licences in sections 2 and 3.

b. You are legally entitled to grant those licences. If Your employer has
   rights to intellectual property You create — which is common — You
   represent that You have received permission to make the Contribution on
   behalf of that employer, that Your employer has waived such rights, or that
   Your employer has signed this agreement as a legal entity under section 5.

c. Your Contributions do not, to the best of Your knowledge, infringe anyone
   else's copyright, patent, trade mark, or trade secret rights.

d. Any Contribution that is not Your original creation is submitted separately
   from Your original Contributions, is identified as such in the submission,
   and carries the complete details of its source and of any licence or other
   restriction attached to it — including a licence that the Project could not
   accept. Third-party material that arrives silently is the one thing this
   agreement cannot survive.

### 5. Legal entities

If You are signing on behalf of a legal entity, You represent that You are
authorised to bind that entity to this agreement, and "You" in every section
above means that entity. The entity should keep a record of which of its
people are authorised to submit Contributions on its behalf, and tell the
Maintainer if that changes.

### 6. No obligation

You understand that the decision to include a Contribution in the Project is
entirely at the Maintainer's discretion, and that this agreement does not
oblige the Maintainer to use, merge, or keep any Contribution. Nothing here
creates an employment, partnership, agency, or joint-venture relationship, or
any expectation of payment.

### 7. No warranty

Unless required by applicable law or agreed in writing, You provide Your
Contributions **"AS IS", WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND**,
either express or implied, including without limitation any warranty of title,
non-infringement, merchantability, or fitness for a particular purpose. You
are not expected to provide support for Your Contributions, though You may do
so if You wish.

### 8. Keeping it accurate

You agree to notify the Maintainer of any fact or circumstance You become
aware of that would make any representation in this agreement inaccurate.

### 9. Governing law

This agreement is governed by the laws of **Ireland**, without regard to its
conflict-of-law provisions, and the courts of Ireland have exclusive
jurisdiction over disputes arising from it.

If any provision of this agreement is held unenforceable, the rest remains in
force.

## How to sign

Add a `Signed-off-by` trailer to every commit You contribute:

```
Signed-off-by: Your Name <your.email@example.com>
```

`git commit -s` writes it for You. Configure `git config user.name` and
`git config user.email` with Your real name and a working email address first;
an anonymous signature is not one.

**By adding that trailer, You state that You have read this agreement and
agree to it, in the version current at the time of the commit, for the
Contribution that commit contains.** That is the whole ceremony — there is no
form to post and no bot to wait for.

If You are contributing on behalf of an employer under section 5, say so once
in the pull request, naming the entity, so that the record is not just a
personal email address.

## Changes to this agreement

The Maintainer may publish a new version of this agreement for future
Contributions. A new version never applies retroactively: Contributions
already made stay under the version in force when they were made, which the
commit's date and this file's history together establish. Version 1.0 is the
version in force.
