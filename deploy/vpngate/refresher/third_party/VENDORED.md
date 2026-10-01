# Vendored VPN Gate exporter

Copied unchanged from any-auto-register at commit `28d4db6`
(`tools/vpngate_openvpn_export.py`, `core/openvpn_config.py`,
`tests/test_vpngate_openvpn_export.py`). `tools/__init__.py` and
`core/__init__.py` are empty, as they are in the source repository.

Do not edit these files here. To update, copy all three again from one
commit of any-auto-register, record that commit above, and run:

    /path/to/python-with-pyyaml-and-pytest -m pytest -p no:cacheprovider \
      --import-mode=importlib deploy/vpngate/refresher -q
