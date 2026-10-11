#!/usr/bin/env python3
"""AF_WORKSPACE_DEK on the ECS control-plane task, checked offline against the template.

    python3 deploy/local/cfn-workspace-dek-test.py

## Why this exists

The per-home credential-store key (#1646) is switched on with AF_WORKSPACE_DEK=random, which
the CP refuses at boot unless the custodian is kms (control-plane/dek.go). On ECS a task that
refuses to boot is restarted forever, so 30-ingress must refuse the combination itself:
WorkspaceDekRandom=on without CustodianKmsKeyArn is a Rules failure at change-set time.
This evaluates the template's Rules, Conditions and the task's Environment for every
combination of the two parameters and fails when

  - the Rule accepts "on" without a key, or refuses a valid combination;
  - AF_WORKSPACE_DEK appears in the CP environment in any case but (on, key set), or with a
    value other than the one the CP accepts;
  - the Go side stops spelling the variable, the value or the refusal the way the template
    does;
  - the parameter stops defaulting to "off" (a stack update must not turn the key on).

Positive controls at the end run the same checks against broken copies of the template.

exit 0 pass, 1 a check failed, 2 the check could not run.
"""
import copy
import os
import re
import sys

try:
    import yaml
except ImportError:
    sys.exit("cfn-workspace-dek-test: PyYAML is required (pip install pyyaml)")

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
TEMPLATE = os.path.join(ROOT, "deploy", "aws", "ecs", "cfn", "30-ingress.yaml")
DEK_GO = os.path.join(ROOT, "control-plane", "dek.go")
KEY = "arn:aws:kms:ap-northeast-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab"


class CfnLoader(yaml.SafeLoader):
    pass


def _intrinsic(tag):
    def construct(loader, node):
        if isinstance(node, yaml.ScalarNode):
            return {"!" + tag: loader.construct_scalar(node)}
        if isinstance(node, yaml.SequenceNode):
            return {"!" + tag: loader.construct_sequence(node, deep=True)}
        return {"!" + tag: loader.construct_mapping(node, deep=True)}
    return construct


for _t in ("Ref", "Sub", "GetAtt", "ImportValue", "Select", "Split", "If", "Equals", "Not",
           "Join", "FindInMap", "Base64", "Condition", "And", "Or", "GetAZs", "Cidr"):
    CfnLoader.add_constructor("!" + _t, _intrinsic(_t))


def load():
    with open(TEMPLATE) as f:
        return yaml.load(f, Loader=CfnLoader)


class Ev:
    """Just enough of CloudFormation's condition functions for this template."""

    def __init__(self, tpl, params):
        defaults = {k: v.get("Default", "") for k, v in tpl["Parameters"].items()}
        self.tpl, self.params, self.conds = tpl, {**defaults, **params}, tpl.get("Conditions", {})

    def val(self, x):
        if isinstance(x, dict) and "!Ref" in x:
            return self.params[x["!Ref"]]
        return x

    def cond(self, x):
        (op, arg), = x.items()
        if op == "!Equals":
            return self.val(arg[0]) == self.val(arg[1])
        if op == "!Not":
            return not self.cond(arg[0])
        if op == "!And":
            return all(self.cond(a) for a in arg)
        if op == "!Or":
            return any(self.cond(a) for a in arg)
        if op == "!Condition":
            return self.cond(self.conds[arg])
        raise SystemExit("cfn-workspace-dek-test: unsupported condition %r" % op)

    def rules_ok(self):
        for name, rule in self.tpl.get("Rules", {}).items():
            if "RuleCondition" in rule and not self.cond(rule["RuleCondition"]):
                continue
            for a in rule["Assertions"]:
                if not self.cond(a["Assert"]):
                    return False
        return True

    def cp_env_names(self):
        """Environment names of the CP container whose If-condition holds."""
        td = self.tpl["Resources"]["TaskDef"]["Properties"]
        env = None
        for c in td["ContainerDefinitions"]:
            if c.get("Name") == "cp":
                env = c["Environment"]
        if env is None:
            raise SystemExit("cfn-workspace-dek-test: no container named cp in TaskDef")
        out = {}
        for e in env:
            if "!If" in e:
                c, yes, no = e["!If"]
                e = yes if self.cond({"!Condition": c}) else no
            if e == {"!Ref": "AWS::NoValue"}:
                continue
            if not (isinstance(e, dict) and "Name" in e):
                raise SystemExit("cfn-workspace-dek-test: unexpected Environment entry %r" % (e,))
            out[e["Name"]] = e["Value"]
        return out


def check(tpl):
    fails = []
    p = tpl["Parameters"].get("WorkspaceDekRandom") or {}
    if p.get("Default") != "off" or p.get("AllowedValues") != ["off", "on"]:
        fails.append("WorkspaceDekRandom must default to off with AllowedValues [off, on]: %r" % p)
    for dek in ("off", "on"):
        for key in ("", KEY):
            ev = Ev(tpl, {"WorkspaceDekRandom": dek, "CustodianKmsKeyArn": key})
            want_ok = not (dek == "on" and key == "")
            if ev.rules_ok() != want_ok:
                fails.append("Rules verdict for WorkspaceDekRandom=%s key=%s: want accepted=%s"
                             % (dek, "set" if key else "empty", want_ok))
            # Evaluated for the refused combination too: the Condition must hold on its own
            # should the Rule ever be dropped.
            env = ev.cp_env_names()
            got = env.get("AF_WORKSPACE_DEK")
            want = "random" if (dek == "on" and key) else None
            if got != want:
                fails.append("AF_WORKSPACE_DEK=%r for WorkspaceDekRandom=%s key=%s, want %r"
                             % (got, dek, "set" if key else "empty", want))
    return fails


def check_go():
    src = open(DEK_GO).read()
    fails = []
    for needle in ('"random"', "AF_WORKSPACE_DEK=random needs AF_KEY_CUSTODIAN=kms"):
        if needle not in src:
            fails.append("control-plane/dek.go no longer contains %s" % needle)
    return fails


def main():
    tpl = load()
    fails = check(tpl) + check_go()
    if fails:
        for f in fails:
            print("FAIL:", f)
        return 1

    # Positive controls: each broken copy must be caught.
    def broken(mutate):
        t = copy.deepcopy(tpl)
        mutate(t)
        return check(t)

    controls = {
        "rule removed": lambda t: t.pop("Rules"),
        "env always set": lambda t: t["Conditions"].__setitem__(
            "UseWorkspaceDek", {"!Equals": ["a", "a"]}),
        "env ignores the key": lambda t: t["Conditions"].__setitem__(
            "UseWorkspaceDek", {"!Equals": [{"!Ref": "WorkspaceDekRandom"}, "on"]}),
        "default on": lambda t: t["Parameters"]["WorkspaceDekRandom"].__setitem__("Default", "on"),
    }
    bad = copy.deepcopy(tpl)
    for c in bad["Resources"]["TaskDef"]["Properties"]["ContainerDefinitions"]:
        if c.get("Name") == "cp":
            for e in c["Environment"]:
                if "!If" in e and e["!If"][0] == "UseWorkspaceDek":
                    e["!If"][2] = {"invalid": "kept"}
    try:
        Ev(bad, {}).cp_env_names()
        fails.append("positive control 'malformed false branch' was not caught")
    except SystemExit:
        pass
    for name, m in controls.items():
        if not broken(m):
            fails.append("positive control %r was not caught" % name)

    if fails:
        for f in fails:
            print("FAIL:", f)
        return 1
    print("cfn-workspace-dek-test: ok (%d positive controls caught)" % len(controls))
    return 0


if __name__ == "__main__":
    sys.exit(main())
