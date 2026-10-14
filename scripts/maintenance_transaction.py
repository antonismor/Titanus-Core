#!/usr/bin/env python3
"""Recoverable scale-to-zero maintenance using the official Realm APIs.

Callbacks must discover the leader for each request. Never automatically replay
an ambiguous POST: rollback reads the current desired state before restoring it.
A durable private journal is written before the first mutation.
"""
import copy
import json
import os
from pathlib import Path
import time

class Maintenance:
    def __init__(self, journal, state, scale, put_fleet, start_services=lambda: None,
                 stop_services=lambda: None, verify=lambda: None,
                 timeout=120, poll=2):
        self.path = Path(journal)
        self.state, self.scale, self.put_fleet = state, scale, put_fleet
        self.start_services, self.stop_services = start_services, stop_services
        self.verify, self.timeout, self.poll = verify, timeout, poll

    def save(self, journal):
        self.path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        tmp = self.path.with_name(self.path.name + ".next")
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w") as out:
            json.dump(journal, out)
            out.flush()
            os.fsync(out.fileno())
        os.replace(tmp, self.path)
        directory = os.open(self.path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)

    def wait(self, predicate, description):
        deadline = time.monotonic() + self.timeout
        while not predicate(self.state()):
            if time.monotonic() >= deadline:
                raise RuntimeError(description)
            time.sleep(self.poll)

    def quiesce(self, names):
        # Refuse a second transaction until the previous one is recovered.
        if self.path.exists() and json.loads(self.path.read_text())["phase"] != "online":
            raise RuntimeError("unfinished maintenance journal; resume it first")
        state = self.state()
        fleets = {name: {"instances": state["fleets"][name]["instances"],
                         "minimum_available": state["fleets"][name].get("minimum_available", 0)}
                  for name in names}
        journal = {"phase": "scaling", "fleets": fleets,
                   "owner_pid": os.getpid(),
                   "owner_start": Path("/proc/self/stat").read_text().split()[21]}
        self.save(journal)
        try:
            for name in names:
                self.scale(name, 0)
            self.wait(lambda s: not any(a["fleet"] in fleets
                       for a in s.get("assignments", {}).values()),
                      "assignments did not quiesce")
            # STOPPED assignments still hold ownership: only the reconciler's
            # acknowledged release/deletion counts as quiescent.
            journal["phase"] = "stopping_services"
            self.save(journal)
            self.stop_services()
            journal["phase"] = "quiescent"
            self.save(journal)
        except BaseException as original:
            try:
                self.resume()
            except BaseException as recovery:
                raise RuntimeError("maintenance failed and recovery is pending in " +
                                   str(self.path) + ": " + str(recovery)) from original
            raise

    def resume(self):
        journal = json.loads(self.path.read_text())
        if journal["phase"] == "online":
            return
        journal["phase"] = "resuming"
        self.save(journal)
        self.start_services()
        # Restore both counts and availability policy: ScaleFleet clamps the
        # minimum to zero, so scaling back alone would silently weaken policy.
        for name, saved in journal["fleets"].items():
            old = self.state()["fleets"][name]
            current = copy.deepcopy(old)
            current["instances"] = saved["instances"]
            current["minimum_available"] = saved["minimum_available"]
            if (old["instances"], old.get("minimum_available", 0)) != (saved["instances"], saved["minimum_available"]):
                self.put_fleet(current)
        def active(s):
            for name, saved in journal["fleets"].items():
                assignments = [a for a in s.get("assignments", {}).values()
                               if a["fleet"] == name]
                if len(assignments) != saved["instances"] or any(
                        a["state"] != "ACTIVE" for a in assignments):
                    return False
            return True
        self.wait(active, "restored assignments did not become ACTIVE")
        deadline = time.monotonic() + self.timeout
        while True:
            try:
                self.verify()
                break
            except Exception:
                if time.monotonic() >= deadline:
                    raise
                time.sleep(self.poll)
        journal["phase"] = "online"
        self.save(journal)

    def recover_if_orphaned(self):
        if not self.path.exists():
            return False
        journal = json.loads(self.path.read_text())
        if journal["phase"] == "online":
            return False
        try:
            start = Path("/proc", str(journal.get("owner_pid")), "stat").read_text().split()[21]
        except FileNotFoundError:
            start = None
        if start is not None and start == journal.get("owner_start"):
            return False
        self.resume()
        return True

    def run(self, names, offline_action):
        self.quiesce(names)
        try:
            return offline_action()
        finally:
            self.resume()
