import importlib.util
import pathlib
import sys
import types
import unittest
from unittest import mock


class FakeText:
    @staticmethod
    def get_character_count(_interface):
        return 5

    @staticmethod
    def get_text(_interface, _start, _end):
        return "hello"


class FakeEditableText:
    @staticmethod
    def insert_text(_interface, _offset, _text, _length):
        return True

    @staticmethod
    def set_text_contents(_interface, _text):
        return True


def load_runtime():
    gi = types.ModuleType("gi")
    gi.require_version = lambda *_args: None
    repository = types.ModuleType("gi.repository")
    repository.Atspi = types.SimpleNamespace(
        Text=FakeText,
        EditableText=FakeEditableText,
    )
    repository.Gdk = types.SimpleNamespace()
    gi.repository = repository

    runtime_path = pathlib.Path(__file__).with_name("runtime.py")
    spec = importlib.util.spec_from_file_location("open_computer_use_linux_runtime", runtime_path)
    module = importlib.util.module_from_spec(spec)
    with mock.patch.dict(
        sys.modules,
        {"gi": gi, "gi.repository": repository},
    ):
        spec.loader.exec_module(module)
    return module


class InterfaceOnlyAccessible:
    def __init__(self, interfaces):
        self.interfaces = interfaces

    def get_interfaces(self):
        return self.interfaces

    def get_text_iface(self):
        return self

    def get_editable_text_iface(self):
        return self

    def get_child_count(self):
        return 0


class FakeNode:
    """A minimal AT-SPI node exposing only role and name, as the verification
    helper reads them through node_role/node_name (which go through safe())."""

    def __init__(self, role, name):
        self._role = role
        self._name = name

    def get_role_name(self):
        return self._role

    def get_name(self):
        return self._name


class RuntimeInterfaceDetectionTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.runtime = load_runtime()

    def test_text_value_uses_accessible_interfaces(self):
        node = InterfaceOnlyAccessible(["Accessible", "Text"])

        self.assertEqual(self.runtime.text_value(node), "hello")

    def test_insert_text_uses_accessible_interfaces(self):
        node = InterfaceOnlyAccessible(["Accessible", "Text", "EditableText"])

        self.assertTrue(self.runtime.insert_text(node, "hello"))

    def test_set_value_uses_accessible_interfaces(self):
        node = InterfaceOnlyAccessible(["Accessible", "Text", "EditableText"])

        self.assertTrue(self.runtime.set_element_value(node, "hello"))


class ElementExpectationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.runtime = load_runtime()

    def test_match_is_not_a_mismatch(self):
        node = FakeNode("push button", "Save")

        self.assertFalse(
            self.runtime.expectation_mismatch(node, "push button", "Save")
        )

    def test_role_mismatch(self):
        node = FakeNode("check box", "Save")

        self.assertTrue(
            self.runtime.expectation_mismatch(node, "push button", "Save")
        )

    def test_name_mismatch(self):
        node = FakeNode("push button", "Cancel")

        self.assertTrue(
            self.runtime.expectation_mismatch(node, "push button", "Save")
        )

    def test_missing_node_with_expectation_is_mismatch(self):
        self.assertTrue(
            self.runtime.expectation_mismatch(None, "push button", "Save")
        )

    def test_no_expectation_is_backward_compatible(self):
        # Legacy invocations carry no expectations, so nothing is checked even for
        # a missing node: behavior is identical to before the modern era.
        self.assertFalse(self.runtime.expectation_mismatch(None, None, None))
        self.assertFalse(self.runtime.expectation_mismatch(None, "", ""))
        node = FakeNode("check box", "Cancel")
        self.assertFalse(self.runtime.expectation_mismatch(node, "", ""))

    def test_role_only_expectation_ignores_name(self):
        node = FakeNode("push button", "whatever")

        self.assertFalse(self.runtime.expectation_mismatch(node, "push button", ""))


class NoActionsNode:
    """A node exposing zero secondary actions, used to reach the post-loop
    'not a valid secondary action' raise in invoke_secondary_action."""

    def get_n_actions(self):
        return 0


class RejectedBeforeInputTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.runtime = load_runtime()

    def test_marker_is_runtime_error_subclass(self):
        self.assertTrue(
            issubclass(self.runtime.RejectedBeforeInput, RuntimeError)
        )

    def test_screen_point_without_point_is_rejected_before_input(self):
        # No element frame and no x/y: a pre-input raise (no clickable point).
        with self.assertRaises(self.runtime.RejectedBeforeInput):
            self.runtime.screen_point({"x": 0, "y": 0, "width": 10, "height": 10})

    def test_missing_element_is_rejected_before_input(self):
        # element None reaches the pre-input 'unknown element_index' raise.
        with self.assertRaises(self.runtime.RejectedBeforeInput):
            self.runtime.invoke_secondary_action(None, "activate")

    def test_post_input_uncertainty_is_unmarked(self):
        # The 'not a valid secondary action' raise is shared with a path that runs
        # after a do_action attempt (matched action, do_action returns False), so
        # it stays a plain RuntimeError. Go then treats the outcome as uncertain
        # (fail-safe), never rejected_before_input.
        node = NoActionsNode()
        with self.assertRaises(RuntimeError) as ctx:
            self.runtime.invoke_secondary_action(node, "activate")
        self.assertNotIsInstance(ctx.exception, self.runtime.RejectedBeforeInput)


if __name__ == "__main__":
    unittest.main()
