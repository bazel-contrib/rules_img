Strategy:

- Download any root manifests that are not in facts storage and keep track if created repos
- Derive full list of manifest blob repos
- Create repos for all manifest blobs (only missing)

- Derive list of config blobs needed (use facts if available)
- Derive list of layer blobs (with categories lazy,eager,shallow)

- Create repos for config blobs
- Create repos for layers??

- Create base image repos with symlinks to other repos and proper BUILD files. Share whenever possible!

- Store facts:
    - "mfst:sha256:...": {
        "mfsts": ["sha256:..."],
        "config": "sha256:...",
        "layers": ["sha256:"].
    }
    - "cas:sha256:...": "{...}"

"trvial repo" optimization: ask in Slack if this is possible:
Assuming we have a trvial repo containing just a source file (and export_files).

Assume we have a rule somewhere else that depends on a target from that repo using an attr with attr.label(allow_single_file). (allow_source_file???)
Can we somehow avoid materializing the repository unless it's actually used as an action input?
