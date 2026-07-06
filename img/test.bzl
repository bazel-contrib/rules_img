"""Public API for testing container images.

```python
load("@rules_img//img:test.bzl", "image_structure_test")
```
"""

load("//img/private:image_structure_test.bzl", _image_structure_test = "image_structure_test")

image_structure_test = _image_structure_test
