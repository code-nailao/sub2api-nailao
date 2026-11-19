import unittest
from audit_gate import validate_report


def summary(**changes):
    value = {'low': 0, 'moderate': 0, 'high': 0, 'critical': 0}
    value.update(changes)
    return value


class AuditGateTests(unittest.TestCase):
    def test_complete_report_accepted(self):
        validate_report({'advisories': {}, 'metadata': {'vulnerabilities': summary()}})

    def test_registry_or_network_error_is_not_success(self):
        with self.assertRaises(ValueError):
            validate_report({'error': {'code': 'ENOTFOUND'}})
        with self.assertRaises(ValueError):
            validate_report({'error': {'code': 'ENOTFOUND'}, 'advisories': {},
                             'metadata': {'vulnerabilities': summary()}})

    def test_missing_summary_rejected(self):
        with self.assertRaises(ValueError):
            validate_report({'advisories': {}})
        with self.assertRaises(ValueError):
            validate_report({'advisories': {}, 'metadata': {'vulnerabilities': {'low': 0, 'high': 0}}})
        with self.assertRaises(ValueError):
            validate_report({'advisories': {}, 'metadata': {'vulnerabilities': summary(critical='0')}})

    def test_non_object_report_rejected(self):
        for value in (None, [], 'ok', {'advisories': []}):
            with self.subTest(value=value), self.assertRaises(ValueError):
                validate_report(value)


if __name__ == '__main__':
    unittest.main()
