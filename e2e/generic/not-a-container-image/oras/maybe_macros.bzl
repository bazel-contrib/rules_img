"""Compatibility shim for ORAS macros when symbolic macros are unavailable."""

load("@rules_img//img:oras.bzl", _oras_file_layer = "oras_file_layer", _oras_layer = "oras_layer")

def missing_symbolic_macros(name, **_kwargs):
    # buildifier: disable=print
    print("Skipping", name, "since symbolic macros are unavailable")

have_macros = _oras_file_layer != None and _oras_layer != None
oras_file_layer = _oras_file_layer if _oras_file_layer != None else missing_symbolic_macros
oras_layer = _oras_layer if _oras_layer != None else missing_symbolic_macros

def maybe_layers(layers):
    if have_macros:
        return layers
    return []
